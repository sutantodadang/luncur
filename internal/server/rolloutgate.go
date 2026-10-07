package server

// Rollout gate: a deploy is `live` only once its new pods serve.
//
// kube.Apply returning means the API server accepted the new pod template,
// not that it runs. With maxUnavailable 0 the old ReplicaSet keeps serving
// while a broken image crash-loops beside it, so marking `live` at apply
// time recorded broken deploys as live (and made them the next rollback
// target). afterApply instead keeps Deployment-backed deploys `deploying`
// and hands them to watchRollout, which marks them live when the rollout is
// done, or failed (and, when the app's policy allows, rolls back) on the
// first fast-fail signal or the rollout timeout.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

// rolloutPoll is the gate's sampling interval; tests shorten it.
var rolloutPoll = 2 * time.Second

// autoRollbackActor is the audit/transcript identity of gate rollbacks.
const autoRollbackActor = "luncur (auto-rollback)"

// gatedKind reports whether an app kind renders a Deployment to gate.
func gatedKind(kind string) bool {
	switch kind {
	case "", "web", "worker", "model":
		return true
	}
	return false
}

// afterApply finishes a deploy whose manifests were just applied: gated
// kinds hand off to watchRollout (the deploy stays `deploying`), everything
// else — and clusters where rollouts can't be watched — goes live now.
func (s *server) afterApply(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment) {
	if !gatedKind(a.Kind) || !s.kube.CanWatchRollouts() {
		s.markDeployLive(ctx, p, env, a, d)
		return
	}
	if _, err := s.kube.RolloutStatus(ctx, env.Namespace, s.rolloutTarget(a, d)); errors.Is(err, kube.ErrNoDeployment) {
		// Nothing to watch (e.g. a clientset that doesn't see the applied
		// objects): keep the pre-gate behavior rather than hang.
		s.markDeployLive(ctx, p, env, a, d)
		return
	}
	go s.watchRollout(p, env, a, d)
}

// rolloutTarget is the Deployment a deploy's gate watches: the canary
// track while a canary/blue-green rollout is starting, the app's own
// otherwise.
func (s *server) rolloutTarget(a store.App, d store.Deployment) string {
	if ro, err := s.st.GetRollout(d.ID); err == nil && ro.Phase == "starting" {
		return canaryName(a.Name)
	}
	return a.Name
}

// gateFailed routes a gate failure: a canary track that never became
// ready aborts the canary (stable untouched); a failed promotion cleans up
// and fails like any rollout (auto-rollback applies).
func (s *server) gateFailed(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment, reason string, mayRollback bool) {
	if ro, err := s.st.GetRollout(d.ID); err == nil {
		switch ro.Phase {
		case "starting", "stepping", "promote-requested", "abort-requested":
			s.abortCanary(ctx, p, env, a, d, reason)
			return
		case "promoting":
			s.cleanupCanary(ctx, env, a)
			s.st.SetRolloutPhase(d.ID, "aborted", reason)
		}
	}
	s.failDeploy(ctx, p, env, a, d, reason, mayRollback)
}

// gateDone routes a finished rollout: a ready canary track starts the
// step machine; a finished promotion removes the canary and goes live.
func (s *server) gateDone(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment) {
	if ro, err := s.st.GetRollout(d.ID); err == nil {
		switch ro.Phase {
		case "starting":
			s.startCanarySteps(p, env, a, d)
			return
		case "promoting":
			s.finishCanary(ctx, env, a, d)
		}
	}
	s.markDeployLive(ctx, p, env, a, d)
}

// watchRollout samples the rollout until it is done, fails, or the deploy
// is superseded. It owns its own context: the request or build that
// applied the deploy may already be gone.
func (s *server) watchRollout(p store.Project, env store.Environment, a store.App, d store.Deployment) {
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		pol = store.DefaultAppPolicy(a.ID)
	}
	timeout := time.Duration(pol.RolloutTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Minute)
	defer cancel()
	started := time.Now()
	target := s.rolloutTarget(a, d)
	s.buildLogf(d, "rollout: waiting up to %s for the new pods to become ready", timeout)
	for {
		if latest, err := s.st.LatestDeployment(a.ID); err == nil && latest.ID != d.ID {
			s.gateFailed(ctx, p, env, a, d, fmt.Sprintf("superseded by deploy #%d before it became ready", latest.Seq), false)
			return
		}
		r, err := s.kube.RolloutStatus(ctx, env.Namespace, target)
		switch {
		case errors.Is(err, kube.ErrNoDeployment):
			s.gateFailed(ctx, p, env, a, d, "the app's Deployment was deleted during the rollout", false)
			return
		case err != nil:
			log.Printf("rollout gate %s/%s #%d: %v", env.Namespace, a.Name, d.Seq, err)
		default:
			ready := 0
			for _, pod := range r.Pods {
				if pod.Ready {
					ready++
				}
			}
			if err := s.st.SetDeployProgress(d.ID, ready, int(r.Want)); err != nil {
				log.Printf("rollout gate: record progress: %v", err)
			}
			v := kube.Judge(r, time.Now(), started, timeout)
			if v.Done {
				s.gateDone(ctx, p, env, a, d)
				return
			}
			if v.Failed {
				s.gateFailed(ctx, p, env, a, d, v.Reason, true)
				return
			}
		}
		select {
		case <-ctx.Done():
			s.gateFailed(context.Background(), p, env, a, d, "rollout gate timed out", true)
			return
		case <-time.After(rolloutPoll):
		}
	}
}

// markDeployLive is the shared success tail of every deploy path.
func (s *server) markDeployLive(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment) {
	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("mark deploy %s live: %v", d.ID, err)
	}
	if err := s.st.SetDeployReady(d.ID, time.Now()); err != nil {
		log.Printf("record deploy %s ready: %v", d.ID, err)
	}
	s.buildLogf(d, "rollout: live")
	// Every successful deploy touches its environment's LastActiveAt so an
	// actively-deployed preview survives reapPreviews' idle-TTL sweep
	// (harmless on a standing environment — nothing reads its LastActiveAt).
	if err := s.st.TouchEnvironment(env.ID); err != nil {
		log.Printf("touch environment %s after deploy: %v", env.Name, err)
	}
	// A successful rollout is the natural moment to clear eviction corpses:
	// the old ReplicaSet's Failed pods are pure noise once the new one is up.
	if s.kube != nil {
		if n, err := s.kube.DeleteFailedPods(ctx, env.Namespace); err != nil {
			log.Printf("gc failed pods after deploy %s: %v", d.ID, err)
		} else if n > 0 {
			log.Printf("deploy %s: deleted %d dead pod(s) in %s", d.ID, n, env.Namespace)
		}
	}
	s.notify(notifyEvent{Event: "deploy_success", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, URL: s.appURLForEnv(a, env.Name, p.DefaultEnv)})
}

// failDeploy marks a gated deploy failed with its reason and, when
// mayRollback and the app's policy allows, rolls back to the previous live
// deploy. A rollback deploy never auto-rolls back (no loops).
func (s *server) failDeploy(ctx context.Context, p store.Project, env store.Environment, a store.App, d store.Deployment, reason string, mayRollback bool) {
	if err := s.st.SetDeploymentStatus(d.ID, "failed"); err != nil {
		log.Printf("mark deploy %s failed: %v", d.ID, err)
	}
	if err := s.st.SetDeployFailReason(d.ID, reason); err != nil {
		log.Printf("record deploy %s fail reason: %v", d.ID, err)
	}
	s.buildLogf(d, "rollout failed: %s", reason)
	s.notify(notifyEvent{Event: "deploy_failed", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, Err: reason})
	if !mayRollback || d.RolledBackFrom != "" {
		return
	}
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil || !pol.AutoRollback {
		return
	}
	cur, err := s.st.GetAppByID(a.ID)
	if err != nil {
		return
	}
	rb, err := s.rollback(ctx, p, env, cur, store.User{Email: autoRollbackActor}, "")
	if err != nil {
		if !errors.Is(err, errNoRollbackTarget) {
			log.Printf("auto-rollback %s/%s after deploy #%d: %v", p.Name, a.Name, d.Seq, err)
		}
		return
	}
	target, _ := s.st.GetDeployment(rb.RolledBackFrom)
	msg := fmt.Sprintf("deploy #%d failed (%s); restoring #%d as deploy #%d", d.Seq, reason, target.Seq, rb.Seq)
	if err := s.st.AppendAudit(autoRollbackActor, "POST /v1/projects/{project}/apps/{app}/rollback", "/v1/projects/"+p.Name+"/apps/"+a.Name+"/rollback"); err != nil {
		log.Printf("audit auto-rollback: %v", err)
	}
	s.notify(notifyEvent{Event: "deploy_rolled_back", Project: p.Name, App: a.Name, DeployID: rb.ID, Seq: rb.Seq, Message: msg})
}

// deployStatusWord is what a deploy endpoint reports right after applying:
// the row's current status (gated deploys are still `deploying`).
func (s *server) deployStatusWord(d store.Deployment) string {
	if cur, err := s.st.GetDeployment(d.ID); err == nil {
		return cur.Status
	}
	return d.Status
}

// failReasonWhy turns a gate fail reason into the 3-line error's "most
// likely why" sentence.
func failReasonWhy(reason string) string {
	switch {
	case strings.HasPrefix(reason, "OOM-killed"):
		return "The container used more memory than its limit; raise it or fix the leak."
	case strings.HasPrefix(reason, "crash-looping"):
		return "The new container exits shortly after starting — usually a missing env var, a bad command, or a failing migration."
	case strings.HasPrefix(reason, "image pull failed"):
		return "The image tag doesn't exist or the registry rejected the pull."
	case strings.HasPrefix(reason, "bad container config"):
		return "A referenced Secret/ConfigMap key or the image name is invalid."
	case strings.HasPrefix(reason, "unschedulable"):
		return "No node has room for the requested CPU, memory or GPU."
	case strings.HasPrefix(reason, "rollout timed out"):
		return "New pods never became ready — check the health path and startup time."
	case strings.HasPrefix(reason, "superseded"):
		return "A newer deploy started before this one finished rolling out."
	}
	return "The rollout did not complete."
}

// rolloutProgress is the Overview pipeline's "ready 1/3" line while a
// deploy is rolling out ("" otherwise).
func (s *server) rolloutProgress(status, deployID string) string {
	if status != "deploying" || deployID == "" {
		return ""
	}
	if ro, err := s.st.GetRollout(deployID); err == nil && ro.Phase == "stepping" {
		name := map[string]string{"canary": "canary", "bluegreen": "blue-green"}[ro.Strategy]
		return fmt.Sprintf("%s · %d%% of traffic on the new image", name, ro.Weight)
	}
	o, err := s.st.GetDeployOutcome(deployID)
	if err != nil || o.Want == 0 {
		return ""
	}
	return fmt.Sprintf("new pods ready %d/%d", o.Ready, o.Want)
}
