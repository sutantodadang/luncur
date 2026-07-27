package server

import (
	"context"
	"log"
	"time"
)

// gcFailedPods sweeps every environment namespace for ReplicaSet-owned
// Failed pods (node-pressure eviction corpses) and deletes them. One
// project's or environment's failure is logged and does not stop the sweep.
func (s *server) gcFailedPods(ctx context.Context) {
	projects, err := s.st.ListProjects()
	if err != nil {
		log.Printf("gc failed pods: list projects: %v", err)
		return
	}
	for _, p := range projects {
		envs, err := s.st.ListEnvironments(p.ID)
		if err != nil {
			log.Printf("gc failed pods: list environments for %s: %v", p.Name, err)
			continue
		}
		for _, env := range envs {
			n, err := s.kube.DeleteFailedPods(ctx, env.Namespace)
			if err != nil {
				log.Printf("gc failed pods: %s/%s: %v", p.Name, env.Name, err)
				continue
			}
			if n > 0 {
				log.Printf("gc failed pods: deleted %d dead pod(s) in %s", n, env.Namespace)
			}
		}
	}
}

// StartFailedPodGC runs the dead-pod sweep hourly until ctx ends: deploys
// clean their own namespace immediately, this loop catches corpses that
// accumulate between deploys (e.g. a node-pressure eviction storm). No-op
// without kube. Mirrors StartPreviewReaper's lifecycle.
func (s *server) StartFailedPodGC(ctx context.Context) {
	if s.kube == nil {
		return
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.gcFailedPods(ctx)
		}
	}
}
