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
// builds for.
func buildJobName(id int64) string { return fmt.Sprintf("build-%d", id) }

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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("build log %d: mkdir: %v", d.ID, err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("build log %d: open: %v", d.ID, err)
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	if _, err := fmt.Fprintf(f, "[luncur] %s %s\n", time.Now().UTC().Format(time.RFC3339), msg); err != nil {
		log.Printf("build log %d: write: %v", d.ID, err)
	}
}

// watchBuildPod polls JobPodStatus every 5s (plus once immediately) and
// appends a milestone line to the deploy log whenever the phase/reason
// changes, including the first observation — this is what lets the UI show
// "builder pod: Pending (ImagePullBackOff)" while the Job is still running,
// well before WaitJob's own poll would notice anything wrong. Stops as soon
// as done is closed; callers close it right after their WaitJob call
// returns.
func (s *server) watchBuildPod(ctx context.Context, d store.Deployment, jobName string, done <-chan struct{}) {
	var last string
	check := func() {
		phase, reason, err := s.kube.JobPodStatus(ctx, s.systemNamespace, jobName)
		if err != nil || phase == "" {
			return
		}
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
	tick := time.NewTicker(5 * time.Second)
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
			log.Printf("build deploy %d failed: %v", d.ID, err)
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
			log.Printf("mark deploy %d failed: %v", d.ID, e)
		}
		s.notify(notifyEvent{Event: "deploy_failed", Project: p.Name, App: a.Name, DeployID: d.ID, Err: err.Error()})
		return err
	}

	if s.src != nil {
		if err := s.st.SetDeploymentLog(d.ID, s.src.LogPath(d.ID)); err != nil {
			log.Printf("set deploy %d log path: %v", d.ID, err)
		}
	}

	imageRef := build.ImageRef(s.registryHost, projectSlug(p), a.Name, d.ID)
	cacheRef := build.CacheRef(s.registryHost, projectSlug(p), a.Name)
	if v, err := s.st.GetSetting("build_cache"); err == nil && v == "off" {
		cacheRef = ""
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
	})
	if err != nil {
		return fail(err)
	}
	if err := s.kube.EnsureNamespace(ctx, s.systemNamespace); err != nil {
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
	if err := s.kube.EnsureNamespace(ctx, p.Namespace); err != nil {
		return err
	}
	if err := s.kube.Apply(ctx, p.Namespace, rendered.Objects); err != nil {
		return err
	}
	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("mark deploy %d live (kube apply already succeeded): %v", d.ID, err)
	}
	s.notify(notifyEvent{Event: "deploy_success", Project: p.Name, App: a.Name, DeployID: d.ID, URL: "http://" + hostFor(a.Name, s.externalIP)})
	return nil
}
