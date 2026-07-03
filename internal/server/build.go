package server

import (
	"context"
	"fmt"
	"log"
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

// startBuild kicks off an async build for a deployment: a Build Job is
// rendered and applied, waited on, and — on success — the app's manifests
// are applied against the resulting image. Errors are logged, not returned;
// callers observe outcome via the deployment row (GET .../deploys/{id}).
func (s *server) startBuild(p store.Project, a store.App, d store.Deployment) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
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
		if e := s.st.SetDeploymentStatus(d.ID, "failed"); e != nil {
			log.Printf("mark deploy %d failed: %v", d.ID, e)
		}
		return err
	}

	if s.src != nil {
		if err := s.st.SetDeploymentLog(d.ID, s.src.LogPath(d.ID)); err != nil {
			log.Printf("set deploy %d log path: %v", d.ID, err)
		}
	}

	imageRef := build.ImageRef(s.registryHost, projectSlug(p), a.Name, d.ID)
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
	})
	if err != nil {
		return fail(err)
	}
	if err := s.kube.EnsureNamespace(ctx, s.systemNamespace); err != nil {
		return fail(err)
	}
	if err := s.kube.Apply(ctx, s.systemNamespace, []render.Object{job}); err != nil {
		return fail(err)
	}

	ok, err := s.kube.WaitJob(ctx, s.systemNamespace, buildJobName(d.ID), 2*time.Second)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(fmt.Errorf("build job failed"))
	}

	if err := s.st.SetDeploymentImage(d.ID, imageRef); err != nil {
		return fail(err)
	}
	if err := s.st.SetDeploymentStatus(d.ID, "deploying"); err != nil {
		return fail(err)
	}

	rendered, err := s.renderApp(p, a, imageRef, true)
	if err != nil {
		return fail(err)
	}
	if err := s.kube.EnsureNamespace(ctx, p.Namespace); err != nil {
		return fail(err)
	}
	if err := s.kube.Apply(ctx, p.Namespace, rendered.Objects); err != nil {
		return fail(err)
	}
	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("mark deploy %d live (kube apply already succeeded): %v", d.ID, err)
	}
	return nil
}
