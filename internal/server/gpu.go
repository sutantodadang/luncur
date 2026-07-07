package server

import (
	"context"
	"log"
	"time"

	"github.com/sutantodadang/luncur/internal/gpu"
)

// gpuWatchInterval is how often StartGPUWatch re-checks for GPU nodes; a
// package-level var so tests can lower it.
var gpuWatchInterval = time.Minute

// StartGPUWatch polls the cluster for GPU-labeled nodes and, when the first
// one appears, applies the nvidia RuntimeClass and the device plugin
// DaemonSet (both idempotent under server-side apply). Runs until ctx ends.
func (s *server) StartGPUWatch(ctx context.Context) {
	if s.kube == nil {
		return
	}
	ensured := false
	check := func() {
		if ensured {
			return
		}
		nodes, err := s.kube.ListNodes(ctx)
		if err != nil {
			return
		}
		hasGPU := false
		for _, n := range nodes {
			if n.GPU {
				hasGPU = true
				break
			}
		}
		if !hasGPU {
			return
		}
		objs, err := gpu.Objects(s.systemNamespace)
		if err != nil {
			log.Printf("render gpu objects: %v", err)
			return
		}
		if err := s.kube.Apply(ctx, s.systemNamespace, objs); err != nil {
			log.Printf("apply gpu device plugin: %v", err)
			return
		}
		log.Printf("gpu node detected: applied nvidia RuntimeClass + device plugin DaemonSet")
		ensured = true
	}
	check()
	tick := time.NewTicker(gpuWatchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			check()
		}
	}
}
