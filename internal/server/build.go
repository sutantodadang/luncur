package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// buildJobName derives the Build Job's name from the deployment id it
// builds for. id is a 12-char [a-z0-9] nanoid, so the result is always a
// valid k8s object name with no further sanitizing needed.
func buildJobName(id string) string { return fmt.Sprintf("build-%s", id) }

// projectSlug returns the DNS-safe project identifier used in image tags.
// Project names are already validated DNS-1123 labels, so this is an
// identity function today, kept as a named seam in case that changes.
func projectSlug(p store.Project) string { return p.Name }

// buildTimeout resolves the configured build_timeout_minutes setting,
// falling back to 15 minutes when it's unset or invalid. Shared by
// startBuild and the restart-reconciliation goroutines (reconcile.go) so a
// re-attached or resumed deploy gets the same budget a fresh one would.
func (s *server) buildTimeout() time.Duration {
	const def = 15 * time.Minute
	v, err := s.st.GetSetting("build_timeout_minutes")
	if err != nil || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return time.Duration(n) * time.Minute
}

// buildLogf appends one timestamped milestone line to a deployment's build
// log, so the UI (which SSE-tails this same file, see handleDeployLogs) has
// something to show before the builder pod exists, and so a server restart
// leaves a trail explaining what reconciliation did. Best-effort: a logging
// failure never fails a build, it's only ever log.Printf'd.
func (s *server) buildLogf(d store.Deployment, format string, args ...any) {
	if s.src == nil {
		return
	}
	path := s.src.LogPath(d.ID)
	// The builder pod appends to this same file as uid/gid 1000 (rootless
	// BuildKit) while the server typically runs as root — without shared
	// write access the builder's tee degrades to stdout-only and the UI log
	// pane never sees builder output. Group-writable to gid 1000, not
	// world-writable (setgid dir so builder-created files inherit the
	// group). Chown/Chmod are best-effort: they fail on non-Linux dev
	// machines where no builder pod exists anyway.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		log.Printf("build log %s: mkdir: %v", d.ID, err)
		return
	}
	if err := os.Chown(dir, -1, build.BuilderGID); err != nil {
		log.Printf("build log %s: chown dir: %v", d.ID, err)
	}
	if err := os.Chmod(dir, 0o2770); err != nil {
		log.Printf("build log %s: chmod dir: %v", d.ID, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o660)
	if err != nil {
		log.Printf("build log %s: open: %v", d.ID, err)
		return
	}
	defer f.Close()
	if err := f.Chown(-1, build.BuilderGID); err != nil {
		log.Printf("build log %s: chown: %v", d.ID, err)
	}
	if err := f.Chmod(0o660); err != nil {
		log.Printf("build log %s: chmod: %v", d.ID, err)
	}
	msg := fmt.Sprintf(format, args...)
	if _, err := fmt.Fprintf(f, "[luncur] %s %s\n", time.Now().UTC().Format(time.RFC3339), msg); err != nil {
		log.Printf("build log %s: write: %v", d.ID, err)
	}
}

// emptyPodChecksBeforeJobEvents is how many consecutive JobPodStatus checks
// with no pod and no error must elapse (counting the immediate check) before
// watchBuildPod asks JobEvents why the Job hasn't produced a pod yet — 7
// checks at the 5s tick interval is ~30s. A package-level var, not a const,
// so tests can lower it and drive the diagnostic without a real 30s wait.
var emptyPodChecksBeforeJobEvents = 7

// jobEventsReemitInterval throttles watchBuildPod's "no builder pod created
// yet" diagnostic once it has fired once: while the Job stays podless, a
// fresh JobEvents call is only considered at most this often, and only logged
// again if its evidence actually changed. A package-level var, not a const,
// so tests can lower it.
var jobEventsReemitInterval = 60 * time.Second

// watchBuildPollInterval is how often watchBuildPod polls JobPodStatus
// (beyond its immediate first check). A package-level var, not a const, so
// tests can lower it and drive several polls without a real multi-second
// wait.
var watchBuildPollInterval = 5 * time.Second

// watchBuildPod polls JobPodStatus every 5s (plus once immediately) and
// appends a milestone line to the deploy log whenever the phase/reason
// changes, including the first observation — this is what lets the UI show
// "builder pod: Pending (ImagePullBackOff)" while the Job is still running,
// well before WaitJob's own poll would notice anything wrong.
//
// Two blind spots this also covers: a Job that never creates a pod at all
// (PodSecurity rejection, quota, an admission webhook) previously left the
// log silent forever after "waiting for builder pod" — after
// emptyPodChecksBeforeJobEvents consecutive empty checks it now fetches and
// logs the Job's recent events, re-checking (throttled) while the Job stays
// podless. And a pod-listing error (e.g. missing RBAC) previously returned
// silently every 5s — it's now logged once, and again only if the error
// message changes, so a permanent failure is visible without spamming.
//
// Stops as soon as done is closed; callers close it right after their
// WaitJob call returns.
func (s *server) watchBuildPod(ctx context.Context, d store.Deployment, jobName string, done <-chan struct{}) {
	var (
		last               string
		lastErr            string
		emptyChecks        int
		emptyEvidenceSeen  bool
		lastEmptyLine      string
		lastEmptyCheckTime time.Time
	)

	check := func() {
		phase, reason, err := s.kube.JobPodStatus(ctx, s.systemNamespace, jobName)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				s.buildLogf(d, "pod watcher error: %v", err)
				lastErr = msg
			}
			return
		}
		lastErr = ""

		if phase == "" {
			emptyChecks++
			if emptyChecks < emptyPodChecksBeforeJobEvents {
				return
			}
			now := time.Now()
			if emptyEvidenceSeen && now.Sub(lastEmptyCheckTime) < jobEventsReemitInterval {
				return
			}
			lastEmptyCheckTime = now
			events, evErr := s.kube.JobEvents(ctx, s.systemNamespace, jobName)
			if evErr != nil {
				// Best-effort: try again at the next allowed check.
				return
			}
			current := "no builder pod created yet (no job events reported)"
			if len(events) > 0 {
				current = events[len(events)-1]
			}
			if emptyEvidenceSeen && current == lastEmptyLine {
				return
			}
			lastEmptyLine = current
			emptyEvidenceSeen = true
			if len(events) == 0 {
				s.buildLogf(d, "no builder pod created yet (no job events reported)")
			} else {
				s.buildLogf(d, "no builder pod created yet — job events:")
				for _, e := range events {
					s.buildLogf(d, "%s", e)
				}
			}
			return
		}

		emptyChecks = 0
		emptyEvidenceSeen = false
		cur := phase
		if reason != "" {
			cur = phase + " (" + reason + ")"
		}
		if cur != last {
			s.buildLogf(d, "builder pod: %s", cur)
			last = cur
		}
	}

	check()
	tick := time.NewTicker(watchBuildPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			check()
		}
	}
}

// startBuild kicks off an async build for a deployment: a Build Job is
// rendered and applied, waited on, and — on success — the app's manifests
// are applied against the resulting image. Errors are logged, not returned;
// callers observe outcome via the deployment row (GET .../deploys/{id}).
func (s *server) startBuild(p store.Project, a store.App, d store.Deployment) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.buildTimeout())
		defer cancel()
		if err := s.runBuild(ctx, p, a, d); err != nil {
			log.Printf("build deploy %s failed: %v", d.ID, err)
		}
	}()
}

// runBuild renders and applies the Build Job into the system namespace,
// waits for it to finish, then (on success) applies the app's manifests
// against the built image into the project namespace.
func (s *server) runBuild(ctx context.Context, p store.Project, a store.App, d store.Deployment) error {
	fail := func(err error) error {
		s.buildLogf(d, "build failed: %v", err)
		if e := s.st.SetDeploymentStatus(d.ID, "failed"); e != nil {
			log.Printf("mark deploy %s failed: %v", d.ID, e)
		}
		s.notify(notifyEvent{Event: "deploy_failed", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, Err: err.Error()})
		return err
	}

	if s.src != nil {
		if err := s.st.SetDeploymentLog(d.ID, s.src.LogPath(d.ID)); err != nil {
			log.Printf("set deploy %s log path: %v", d.ID, err)
		}
	}

	imageRef := build.ImageRef(s.registryHost, projectSlug(p), a.Name, d.Seq)
	cacheRef := build.CacheRef(s.registryHost, projectSlug(p), a.Name)
	if v, err := s.st.GetSetting("build_cache"); err == nil && v == "off" {
		cacheRef = ""
	}

	// A sealed env that can't be unsealed must not silently build with
	// missing build-args — same contract renderApp uses further down this
	// same deploy for runtime env.
	buildEnv, err := s.plainEnv(a)
	if err != nil {
		return fail(err)
	}

	s.buildLogf(d, "rendering build job")
	job, err := build.RenderBuildJob(build.BuildParams{
		Namespace:    s.systemNamespace,
		Name:         buildJobName(d.ID),
		BuilderImage: s.builderImage,
		DataPVC:      s.dataPVC,
		ImageRef:     imageRef,
		RegistryHost: s.registryHost,
		SourceType:   a.SourceType,
		GitURL:       a.GitURL,
		GitBranch:    a.GitBranch,
		DeployID:     d.ID,
		CacheRef:     cacheRef,
		BuildPath:    a.BuildPath,
		BuildEnv:     buildEnv,
	})
	if err != nil {
		return fail(err)
	}
	// privileged, not restricted/baseline: rootless BuildKit needs setuid
	// newuidmap (restricted forbids privilege escalation) AND unconfined
	// seccomp/AppArmor for rootlesskit's mount-namespace setup (baseline
	// forbids unconfined profiles; observed on Ubuntu hosts as
	// "rootlesskit: failed to share mount point: /: permission denied").
	// Both confirmed in production. Project/app namespaces stay restricted;
	// only this system namespace, which runs luncur-operated build infra
	// rather than tenant apps, is exempt — the build pod itself still runs
	// as the unprivileged uid 1000 (see RenderBuildJob's SecurityContext).
	if err := s.kube.EnsureNamespaceWithPolicy(ctx, s.systemNamespace, "privileged"); err != nil {
		return fail(err)
	}
	s.buildLogf(d, "applying build job to cluster")
	if err := s.kube.Apply(ctx, s.systemNamespace, []render.Object{job}); err != nil {
		return fail(err)
	}

	s.buildLogf(d, "waiting for builder pod")
	jobName := buildJobName(d.ID)
	watchDone := make(chan struct{})
	go s.watchBuildPod(ctx, d, jobName, watchDone)
	ok, err := s.kube.WaitJob(ctx, s.systemNamespace, jobName, 2*time.Second)
	close(watchDone)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(fmt.Errorf("build job failed"))
	}

	if err := s.finishDeploy(ctx, p, a, d, imageRef); err != nil {
		return fail(err)
	}
	return nil
}

// finishDeploy applies a built image's manifests to the cluster and marks
// the deployment live: the shared tail end of both runBuild's
// Job-succeeded path and reconcileUnfinished's resume path (a 'deploying'
// deployment already has image_ref set — re-setting it here is harmless).
func (s *server) finishDeploy(ctx context.Context, p store.Project, a store.App, d store.Deployment, imageRef string) error {
	if err := s.st.SetDeploymentImage(d.ID, imageRef); err != nil {
		return err
	}
	if err := s.st.SetDeploymentStatus(d.ID, "deploying"); err != nil {
		return err
	}

	rendered, err := s.renderApp(p, a, imageRef, true)
	if err != nil {
		return err
	}
	if err := s.ensureProjectNamespace(ctx, p.Namespace); err != nil {
		return err
	}
	if err := s.kube.Apply(ctx, p.Namespace, rendered.Objects); err != nil {
		return err
	}
	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("mark deploy %s live (kube apply already succeeded): %v", d.ID, err)
	}
	s.notify(notifyEvent{Event: "deploy_success", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, URL: s.appURL(a)})
	return nil
}
