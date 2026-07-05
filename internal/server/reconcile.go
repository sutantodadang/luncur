package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/store"
)

// reconcileUnfinished resolves every deployment a server restart left
// stranded in 'building' or 'deploying' — startBuild's goroutine (and its
// context) died with the previous process, so without this those rows
// would sit stuck forever (see build.go's package doc on startBuild/
// runBuild). Runs once at startup, alongside the server's other background
// loops (see push.go's NewWithBackend).
func (s *server) reconcileUnfinished(ctx context.Context) {
	if s.kube == nil {
		log.Printf("reconcile: no kubernetes client configured, skipping")
		return
	}

	deploys, err := s.st.UnfinishedDeployments()
	if err != nil {
		log.Printf("reconcile: list unfinished deployments: %v", err)
		return
	}
	for _, d := range deploys {
		a, err := s.st.GetAppByID(d.AppID)
		if err != nil {
			log.Printf("reconcile deploy %s: get app %d: %v", d.ID, d.AppID, err)
			continue
		}
		p, err := s.st.GetProjectByID(a.ProjectID)
		if err != nil {
			log.Printf("reconcile deploy %s: get project %d: %v", d.ID, a.ProjectID, err)
			continue
		}

		switch d.Status {
		case "building":
			s.reconcileBuilding(ctx, p, a, d)
		case "deploying":
			s.reconcileDeploying(p, a, d)
		}
	}
}

// reconcileFail marks a reconciled deployment failed and sends the
// deploy_failed notification, mirroring runBuild's fail() closure's
// status/notify behavior (same event shape) — the deploy-log milestone is
// left to the caller since its wording differs per failure mode.
func (s *server) reconcileFail(p store.Project, a store.App, d store.Deployment, err error) {
	if e := s.st.SetDeploymentStatus(d.ID, "failed"); e != nil {
		log.Printf("mark deploy %s failed: %v", d.ID, e)
	}
	s.notify(notifyEvent{Event: "deploy_failed", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, Err: err.Error()})
}

// reconcileBuilding handles a deployment orphaned in 'building': if the
// Build Job it was waiting on is gone (e.g. the Job itself was also lost,
// or never got applied), there's nothing to resume, so it's marked failed
// immediately. If the Job is still there, a goroutine re-attaches to it
// with a fresh buildTimeout budget, exactly like a fresh startBuild would.
func (s *server) reconcileBuilding(ctx context.Context, p store.Project, a store.App, d store.Deployment) {
	jobName := buildJobName(d.ID)
	exists, err := s.kube.JobExists(ctx, s.systemNamespace, jobName)
	if err != nil {
		s.buildLogf(d, "build orphaned by server restart, could not verify build job (%v) — marked failed", err)
		s.reconcileFail(p, a, d, fmt.Errorf("orphaned by server restart: %w", err))
		return
	}
	if !exists {
		s.buildLogf(d, "build orphaned by server restart, no job found — marked failed")
		s.reconcileFail(p, a, d, fmt.Errorf("orphaned by server restart"))
		return
	}

	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), s.buildTimeout())
		defer cancel()

		s.buildLogf(d, "server restarted — re-attached to running build job")
		watchDone := make(chan struct{})
		go s.watchBuildPod(bctx, d, jobName, watchDone)
		ok, err := s.kube.WaitJob(bctx, s.systemNamespace, jobName, 2*time.Second)
		close(watchDone)
		if err != nil {
			s.buildLogf(d, "build failed: %v", err)
			s.reconcileFail(p, a, d, err)
			return
		}
		if !ok {
			err := fmt.Errorf("build job failed")
			s.buildLogf(d, "build failed: %v", err)
			s.reconcileFail(p, a, d, err)
			return
		}

		imageRef := build.ImageRef(s.registryHost, projectSlug(p), a.Name, d.Seq)
		if err := s.finishDeploy(bctx, p, a, d, imageRef); err != nil {
			s.buildLogf(d, "build failed: %v", err)
			s.reconcileFail(p, a, d, err)
		}
	}()
}

// reconcileDeploying handles a deployment orphaned in 'deploying': the
// build already succeeded and image_ref is set, so all that's left is
// finishDeploy's apply-and-mark-live tail, run again from scratch (it's
// idempotent).
func (s *server) reconcileDeploying(p store.Project, a store.App, d store.Deployment) {
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), s.buildTimeout())
		defer cancel()

		s.buildLogf(d, "server restarted — resuming deploy")
		if err := s.finishDeploy(bctx, p, a, d, d.ImageRef); err != nil {
			s.buildLogf(d, "build failed: %v", err)
			s.reconcileFail(p, a, d, err)
		}
	}()
}
