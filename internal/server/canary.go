package server

// Canary and blue-green deploys.
//
// A canary deploy leaves the app's own Deployment ("stable") alone and
// rolls the new image out as a second Deployment, <app>-canary, plus a
// <app>-canary Service. Traffic then shifts in steps:
//
//   - weighted (Traefik CRDs present, as on K3s): a TraefikService splits
//     <app> / <app>-canary by weight, behind IngressRoutes that outrank
//     the app's Ingress for the same hosts. Deleting them hands routing
//     back to the untouched Ingress.
//   - replica ratio (fallback): canary pods also carry the app's name
//     label, so the app's own Service balances across both tracks by pod
//     count; the canary is scaled to approximate each weight.
//
// Each step holds for the canary interval while luncur probes the canary
// Service; a failing success rate or a restarting canary pod aborts
// (stable never changed, so nothing to roll back). After the last step the
// stable Deployment is rolled to the new image (rollout-gated), then the
// canary objects are removed. Blue-green is the same machine with a
// full-size canary and a single 100% step held for bluegreen_keep.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

const (
	canaryTrackLabel = "luncur.dev/track"
	traefikCRD       = "traefikservices.traefik.io"
)

// Test seams: canaryTick paces the step loop and probes; canaryTimeScale
// shrinks step holds.
var (
	canaryTick      = 10 * time.Second
	canaryTimeScale = 1.0
)

func canaryName(app string) string { return app + "-canary" }
func splitName(app string) string  { return app + "-split" }

// deployStrategy picks how this deploy rolls out. Canary/blue-green need a
// web app the policy allows, a live stable deploy to compare against, and
// a watchable cluster; rollbacks always roll (they restore a known-good
// image).
func (s *server) deployStrategy(a store.App, d store.Deployment) string {
	if d.RolledBackFrom != "" || (a.Kind != "" && a.Kind != "web") || a.Internal || !s.kube.CanWatchRollouts() {
		return "rolling"
	}
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil || pol.Strategy == "rolling" || s.strategyError(a, pol.Strategy) != nil {
		return "rolling"
	}
	if _, err := s.st.GetRollout(d.ID); err == nil {
		return pol.Strategy // resuming
	}
	history, err := s.st.ListDeployments(a.ID)
	if err != nil {
		return "rolling"
	}
	for _, h := range history {
		if h.ID != d.ID && h.Status == "live" {
			return pol.Strategy
		}
	}
	return "rolling" // first deploy: nothing to compare against
}

// applyAndGate is the shared apply tail of every deploy path: canary /
// blue-green deploys start their rollout, everything else applies the app
// and hands it to the rollout gate.
func (s *server) applyAndGate(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment, image string) error {
	if strategy := s.deployStrategy(a, d); strategy != "rolling" {
		return s.startCanary(ctx, p, env, a, d, strategy)
	}
	rendered, err := s.renderApp(p, env, a, image, true)
	if err != nil {
		return err
	}
	if err := s.ensureEnvNamespace(ctx, env); err != nil {
		return err
	}
	if err := s.kube.Apply(ctx, env.Namespace, rendered.Objects); err != nil {
		return err
	}
	s.afterApply(ctx, p, env, a, d)
	return nil
}

// startCanary starts (or, after a restart, resumes) a canary rollout.
func (s *server) startCanary(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment, strategy string) error {
	ro, err := s.st.GetRollout(d.ID)
	if errors.Is(err, store.ErrNotFound) {
		ro, err = s.st.CreateRollout(d.ID, a.ID, strategy)
	}
	if err != nil {
		return err
	}
	switch ro.Phase {
	case "starting":
		weighted := s.canaryWeighted(ctx)
		replicas := int32(1)
		if strategy == "bluegreen" || !weighted {
			replicas = s.canaryReplicas(ctx, env, a, strategy, 0, weighted)
		}
		objs, err := s.canaryObjects(p, env, a, d, replicas, weighted)
		if err != nil {
			return err
		}
		if err := s.ensureEnvNamespace(ctx, env); err != nil {
			return err
		}
		if err := s.kube.Apply(ctx, env.Namespace, objs); err != nil {
			return err
		}
		s.buildLogf(d, "%s: canary track %s rolling out (%s traffic)", strategy, canaryName(a.Name), map[bool]string{true: "weighted", false: "replica-ratio"}[weighted])
		go s.watchRollout(p, env, a, d)
	case "promoting":
		return s.promoteCanary(ctx, p, env, a, d)
	case "done", "aborted":
		return nil
	default:
		go s.runCanary(p, env, a, d)
	}
	return nil
}

// canaryWeighted reports whether Traefik's weighted routing CRDs exist.
func (s *server) canaryWeighted(ctx context.Context) bool {
	ok, err := s.kube.HasCRD(ctx, traefikCRD)
	return err == nil && ok
}

// canaryReplicas sizes the canary track: blue-green runs full size; the
// replica-ratio fallback approximates weight w (percent) as
// canary/(stable+canary).
func (s *server) canaryReplicas(ctx context.Context, env store.Environment, a store.App, strategy string, w int, weighted bool) int32 {
	stable := int32(a.Replicas)
	if r, err := s.kube.RolloutStatus(ctx, env.Namespace, a.Name); err == nil && r.Want > 0 {
		stable = r.Want
	}
	if stable < 1 {
		stable = 1
	}
	if strategy == "bluegreen" {
		return stable
	}
	if weighted || w <= 0 {
		return 1
	}
	if w >= 100 {
		return stable
	}
	n := int32(math.Ceil(float64(stable) * float64(w) / float64(100-w)))
	if n < 1 {
		n = 1
	}
	return n
}

// canaryObjects renders the canary track from the app's own manifests at
// the deploy's image: the Deployment renamed and relabeled, a Service for
// it, and the app's env Secret.
func (s *server) canaryObjects(p store.Project, env store.Environment, a store.App, d store.Deployment, replicas int32, weighted bool) ([]render.Object, error) {
	rendered, err := s.renderApp(p, env, a, d.ImageRef, true)
	if err != nil {
		return nil, err
	}
	podLabels := map[string]string{"app.kubernetes.io/name": canaryName(a.Name), "app.kubernetes.io/managed-by": "luncur", canaryTrackLabel: "canary"}
	sel := map[string]string{"app.kubernetes.io/name": canaryName(a.Name)}
	if !weighted {
		// Ratio mode: the app's Service (selector name=<app>) must see the
		// canary pods too.
		podLabels["app.kubernetes.io/name"] = a.Name
		sel = map[string]string{"app.kubernetes.io/name": a.Name, canaryTrackLabel: "canary"}
	}
	var out []render.Object
	for _, o := range rendered.Objects {
		switch o.Kind {
		case "Secret":
			out = append(out, o)
		case "Deployment":
			var dep appsv1.Deployment
			if err := json.Unmarshal(o.JSON, &dep); err != nil {
				return nil, err
			}
			dep.Name = canaryName(a.Name)
			dep.Labels = map[string]string{"app.kubernetes.io/name": canaryName(a.Name), "app.kubernetes.io/managed-by": "luncur", canaryTrackLabel: "canary"}
			dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: sel}
			dep.Spec.Template.Labels = podLabels
			dep.Spec.Replicas = &replicas
			for i := range dep.Spec.Template.Spec.TopologySpreadConstraints {
				dep.Spec.Template.Spec.TopologySpreadConstraints[i].LabelSelector = &metav1.LabelSelector{MatchLabels: sel}
			}
			b, err := json.Marshal(dep)
			if err != nil {
				return nil, err
			}
			out = append(out, render.Object{Kind: "Deployment", JSON: b})
		case "Service":
			var svc corev1.Service
			if err := json.Unmarshal(o.JSON, &svc); err != nil {
				return nil, err
			}
			svc.Name = canaryName(a.Name)
			svc.Labels = map[string]string{"app.kubernetes.io/name": canaryName(a.Name), "app.kubernetes.io/managed-by": "luncur", canaryTrackLabel: "canary"}
			svc.Spec.Selector = sel
			b, err := json.Marshal(svc)
			if err != nil {
				return nil, err
			}
			out = append(out, render.Object{Kind: "Service", JSON: b})
		}
	}
	return out, nil
}

// trafficObjects renders the weighted split: a TraefikService and the
// IngressRoutes (plain and TLS) that send the app's hosts to it.
func (s *server) trafficObjects(p store.Project, env store.Environment, a store.App, d store.Deployment, weight int) ([]render.Object, error) {
	rendered, err := s.renderApp(p, env, a, d.ImageRef, true)
	if err != nil {
		return nil, err
	}
	var hosts []string
	tls := false
	resolver := ""
	for _, o := range rendered.Objects {
		if o.Kind != "Ingress" {
			continue
		}
		var ing netv1.Ingress
		if err := json.Unmarshal(o.JSON, &ing); err != nil {
			return nil, err
		}
		for _, r := range ing.Spec.Rules {
			hosts = append(hosts, r.Host)
		}
		tls = len(ing.Spec.TLS) > 0
		resolver = ing.Annotations["traefik.ingress.kubernetes.io/router.tls.certresolver"]
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("app %s has no public hosts to split", a.Name)
	}
	meta := map[string]any{"name": splitName(a.Name), "labels": map[string]any{"app.kubernetes.io/name": a.Name, "app.kubernetes.io/managed-by": "luncur", canaryTrackLabel: "split"}}
	ts := map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "TraefikService", "metadata": meta,
		"spec": map[string]any{"weighted": map[string]any{"services": []any{
			map[string]any{"name": a.Name, "port": 80, "weight": 100 - weight},
			map[string]any{"name": canaryName(a.Name), "port": 80, "weight": weight},
		}}},
	}
	matches := make([]string, len(hosts))
	for i, h := range hosts {
		matches[i] = "Host(`" + h + "`)"
	}
	route := func(suffix string, entry string, tlsBlock map[string]any) map[string]any {
		m := map[string]any{"name": splitName(a.Name) + "-" + suffix, "labels": meta["labels"]}
		spec := map[string]any{
			"entryPoints": []any{entry},
			"routes": []any{map[string]any{
				"match": strings.Join(matches, " || "), "kind": "Rule", "priority": 10000,
				"services": []any{map[string]any{"name": splitName(a.Name), "kind": "TraefikService"}},
			}},
		}
		if tlsBlock != nil {
			spec["tls"] = tlsBlock
		}
		return map[string]any{"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute", "metadata": m, "spec": spec}
	}
	objs := []map[string]any{ts, route("web", "web", nil)}
	if tls || resolver != "" {
		block := map[string]any{}
		if resolver != "" {
			block["certResolver"] = resolver
		}
		objs = append(objs, route("websecure", "websecure", block))
	}
	out := make([]render.Object, 0, len(objs))
	for _, o := range objs {
		b, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		out = append(out, render.Object{Kind: o["kind"].(string), JSON: b})
	}
	return out, nil
}

// setCanaryWeight sends weight percent of traffic to the canary track.
func (s *server) setCanaryWeight(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment, strategy string, weight int) error {
	if s.canaryWeighted(ctx) {
		objs, err := s.trafficObjects(p, env, a, d, weight)
		if err != nil {
			return err
		}
		return s.kube.Apply(ctx, env.Namespace, objs)
	}
	objs, err := s.canaryObjects(p, env, a, d, s.canaryReplicas(ctx, env, a, strategy, weight, false), false)
	if err != nil {
		return err
	}
	return s.kube.Apply(ctx, env.Namespace, objs)
}

// canarySteps are the weights a rollout walks: the policy's canary steps,
// or a single 100% switch for blue-green.
func canarySteps(pol store.AppPolicy, strategy string) []int {
	if strategy == "bluegreen" {
		return []int{100}
	}
	return pol.CanarySteps
}

func canaryHold(pol store.AppPolicy, strategy string) time.Duration {
	secs := pol.CanaryInterval
	if strategy == "bluegreen" {
		secs = pol.BlueGreenKeep
		if secs < pol.CanaryInterval {
			secs = pol.CanaryInterval
		}
	}
	return time.Duration(float64(time.Duration(secs)*time.Second) * canaryTimeScale)
}

// startCanarySteps is called by the rollout gate once the canary track is
// ready.
func (s *server) startCanarySteps(p store.Project, env store.Environment, a store.App, d store.Deployment) {
	s.buildLogf(d, "canary track ready — shifting traffic")
	go s.runCanary(p, env, a, d)
}

// runCanary drives the step machine until promotion or abort.
func (s *server) runCanary(p store.Project, env store.Environment, a store.App, d store.Deployment) {
	ctx := context.Background()
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		pol = store.DefaultAppPolicy(a.ID)
	}
	for {
		ro, err := s.st.GetRollout(d.ID)
		if err != nil {
			log.Printf("canary %s/%s #%d: %v", p.Name, a.Name, d.Seq, err)
			return
		}
		steps := canarySteps(pol, ro.Strategy)
		if latest, err := s.st.LatestDeployment(a.ID); err == nil && latest.ID != d.ID {
			s.abortCanary(ctx, p, env, a, d, fmt.Sprintf("superseded by deploy #%d", latest.Seq))
			return
		}
		switch ro.Phase {
		case "abort-requested":
			s.abortCanary(ctx, p, env, a, d, "aborted by request")
			return
		case "promote-requested":
			if err := s.promoteCanary(ctx, p, env, a, d); err != nil {
				s.abortCanary(ctx, p, env, a, d, "promote failed: "+err.Error())
			}
			return
		case "starting":
			if err := s.setCanaryWeight(ctx, p, env, a, d, ro.Strategy, steps[0]); err != nil {
				s.abortCanary(ctx, p, env, a, d, "route traffic: "+err.Error())
				return
			}
			if err := s.st.SetRolloutStep(d.ID, 0, steps[0]); err != nil {
				log.Printf("canary %s: record step: %v", d.ID, err)
			}
			s.buildLogf(d, "canary: %d%% of traffic", steps[0])
			continue
		case "done", "aborted", "promoting":
			return
		}

		// stepping: probe, check the canary pods, advance when the hold ends.
		ok := s.canaryProbe(ctx, env, a)
		if err := s.st.AddRolloutProbe(d.ID, ok); err != nil {
			log.Printf("canary %s: record probe: %v", d.ID, err)
		}
		ro, _ = s.st.GetRollout(d.ID)
		if reason := s.canaryUnhealthy(ctx, env, a); reason != "" {
			s.abortCanary(ctx, p, env, a, d, reason)
			return
		}
		rate := 100
		if ro.ProbesTotal > 0 {
			rate = ro.ProbesOK * 100 / ro.ProbesTotal
		}
		if ro.ProbesTotal >= 5 && rate < pol.CanaryMinSuccess {
			s.abortCanary(ctx, p, env, a, d, fmt.Sprintf("canary success rate %d%% < %d%% at %d%% traffic", rate, pol.CanaryMinSuccess, ro.Weight))
			return
		}
		if time.Since(parseDBTime(ro.StepStartedAt)) >= canaryHold(pol, ro.Strategy) {
			if ro.ProbesTotal > 0 && rate < pol.CanaryMinSuccess {
				s.abortCanary(ctx, p, env, a, d, fmt.Sprintf("canary success rate %d%% < %d%% at %d%% traffic", rate, pol.CanaryMinSuccess, ro.Weight))
				return
			}
			if next := ro.Step + 1; next < len(steps) {
				if err := s.setCanaryWeight(ctx, p, env, a, d, ro.Strategy, steps[next]); err != nil {
					s.abortCanary(ctx, p, env, a, d, "route traffic: "+err.Error())
					return
				}
				if err := s.st.SetRolloutStep(d.ID, next, steps[next]); err != nil {
					log.Printf("canary %s: record step: %v", d.ID, err)
				}
				s.buildLogf(d, "canary: %d%% of traffic", steps[next])
				continue
			}
			if err := s.promoteCanary(ctx, p, env, a, d); err != nil {
				s.abortCanary(ctx, p, env, a, d, "promote failed: "+err.Error())
			}
			return
		}
		time.Sleep(canaryTick)
	}
}

// canaryProbe makes one HTTP request to the canary Service (test seam:
// s.canaryProbeFn). Any response below 500 counts as healthy.
func (s *server) canaryProbe(ctx context.Context, env store.Environment, a store.App) bool {
	path := a.HealthPath
	if path == "" {
		path = "/"
	}
	url := "http://" + canaryName(a.Name) + "." + env.Namespace + ".svc.cluster.local" + path
	if s.canaryProbeFn != nil {
		return s.canaryProbeFn(url)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// canaryUnhealthy reports a restarting or crashing canary pod ("" = fine).
func (s *server) canaryUnhealthy(ctx context.Context, env store.Environment, a store.App) string {
	r, err := s.kube.RolloutStatus(ctx, env.Namespace, canaryName(a.Name))
	if err != nil {
		if errors.Is(err, kube.ErrNoDeployment) {
			return "the canary Deployment disappeared"
		}
		return ""
	}
	for _, pod := range r.Pods {
		if pod.Restarts > 0 {
			why := pod.LastTerminated
			if why == "" {
				why = "restarted"
			}
			return fmt.Sprintf("canary pod %s restarted (%s)", pod.Name, why)
		}
	}
	return ""
}

// promoteCanary rolls the stable Deployment to the new image (the rollout
// gate watches it); the gate finishes the canary on success.
func (s *server) promoteCanary(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment) error {
	if err := s.st.SetRolloutPhase(d.ID, "promoting", ""); err != nil {
		return err
	}
	rendered, err := s.renderApp(p, env, a, d.ImageRef, true)
	if err != nil {
		return err
	}
	if err := s.kube.Apply(ctx, env.Namespace, rendered.Objects); err != nil {
		return err
	}
	s.buildLogf(d, "canary healthy — promoting: rolling the stable track to the new image")
	go s.watchRollout(p, env, a, d)
	return nil
}

// finishCanary removes the canary track after a successful promotion.
func (s *server) finishCanary(ctx context.Context, env store.Environment, a store.App, d store.Deployment) {
	s.cleanupCanary(ctx, env, a)
	if err := s.st.SetRolloutPhase(d.ID, "done", ""); err != nil {
		log.Printf("canary %s: mark done: %v", d.ID, err)
	}
}

// abortCanary tears the canary track down and fails the deploy. Stable was
// never changed, so there is nothing to roll back — unless a newer deploy
// already owns the canary objects, which are then left alone.
func (s *server) abortCanary(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment, reason string) {
	if ro, err := s.st.ActiveRollout(a.ID); err != nil || ro.DeployID == d.ID {
		s.cleanupCanary(ctx, env, a)
	}
	if err := s.st.SetRolloutPhase(d.ID, "aborted", reason); err != nil {
		log.Printf("canary %s: mark aborted: %v", d.ID, err)
	}
	s.buildLogf(d, "canary aborted: %s", reason)
	if cur, err := s.st.GetDeployment(d.ID); err == nil && cur.Status == "deploying" {
		s.failDeploy(ctx, p, env, a, d, "canary aborted: "+reason, false)
	}
}

// cleanupCanary deletes the split routing first (traffic returns to the
// app's Ingress → stable), then the canary track.
func (s *server) cleanupCanary(ctx context.Context, env store.Environment, a store.App) {
	for _, o := range []struct{ kind, name string }{
		{"IngressRoute", splitName(a.Name) + "-web"},
		{"IngressRoute", splitName(a.Name) + "-websecure"},
		{"TraefikService", splitName(a.Name)},
		{"Deployment", canaryName(a.Name)},
		{"Service", canaryName(a.Name)},
	} {
		if err := s.kube.DeleteObject(ctx, env.Namespace, o.kind, o.name); err != nil && !kube.IsNotFound(err) {
			log.Printf("canary cleanup %s/%s %s: %v", env.Namespace, o.name, o.kind, err)
		}
	}
}

// parseDBTime parses SQLite's datetime('now') format (UTC).
func parseDBTime(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
	if err != nil {
		return time.Now()
	}
	return t
}

// rolloutView is the API/UI view of an app's current (or last) rollout.
type rolloutView struct {
	store.Rollout
	Seq         int64 `json:"seq"`
	Steps       []int `json:"steps"`
	SuccessRate int   `json:"success_rate"`
	Active      bool  `json:"active"`
}

func (s *server) appRolloutView(a store.App) (rolloutView, bool) {
	ro, err := s.st.ActiveRollout(a.ID)
	if err != nil {
		return rolloutView{}, false
	}
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		pol = store.DefaultAppPolicy(a.ID)
	}
	v := rolloutView{Rollout: ro, Steps: canarySteps(pol, ro.Strategy), SuccessRate: 100, Active: store.RolloutActive(ro.Phase)}
	if ro.ProbesTotal > 0 {
		v.SuccessRate = ro.ProbesOK * 100 / ro.ProbesTotal
	}
	if d, err := s.st.GetDeployment(ro.DeployID); err == nil {
		v.Seq = d.Seq
	}
	return v, true
}

var errNoActiveRollout = errors.New("no canary or blue-green rollout is in progress")

// requestRollout records promote/abort on the app's active rollout.
func (s *server) requestRollout(a store.App, action string) error {
	ro, err := s.st.ActiveRollout(a.ID)
	if err != nil {
		return errNoActiveRollout
	}
	ok, err := s.st.RequestRolloutPhase(ro.DeployID, action+"-requested")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the rollout is already %s", ro.Phase)
	}
	return nil
}

func (s *server) handleGetRollout(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	v, ok := s.appRolloutView(a)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleRolloutAction(action string) func(http.ResponseWriter, *http.Request, store.User) {
	return func(w http.ResponseWriter, r *http.Request, u store.User) {
		p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
		if !ok {
			return
		}
		a, ok := s.requireApp(w, p, env, r.PathValue("app"))
		if !ok {
			return
		}
		if err := s.requestRollout(a, action); err != nil {
			code := http.StatusConflict
			writeError(w, code, "conflict", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": action + "-requested"})
	}
}

// handleUIRolloutAction is handleRolloutAction's UI twin (Ship tab).
func (s *server) handleUIRolloutAction(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "promote" && action != "abort" {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if err := s.requestRollout(a, action); err != nil {
		flash(w, "err", err.Error())
	} else {
		flash(w, "ok", "rollout "+action+" requested")
	}
	uiRedirect(w, r, p, a, tabShip)
}

// uiRollout is the Ship tab's rollout card data (nil when none is active;
// only loaded on Ship and Overview).
func (s *server) uiRollout(a store.App, tab uiTab) *rolloutView {
	if tab != tabShip && tab != tabOverview {
		return nil
	}
	v, ok := s.appRolloutView(a)
	if !ok || !v.Active {
		return nil
	}
	return &v
}
