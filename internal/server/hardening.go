package server

// Workload hardening: the defaults and cluster-level objects behind the
// render.Input hardening knobs (render/harden.go) — requests-only resource
// defaults, the TCP readiness probe, preStop drain, revision history,
// progress deadline, topology spread, priority classes and pod security.

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// Defaults for the workload settings.
const (
	defaultCPURequest    = "50m"
	defaultMemoryRequest = "64Mi"
	preStopSleepSeconds  = 5
	revisionHistoryLimit = 5
)

// Priority classes luncur manages (see render.PriorityClasses).
const (
	priorityClassSystem = render.PriorityClassSystem
	priorityClassApp    = render.PriorityClassApp
	priorityClassBatch  = render.PriorityClassBatch
)

// hardeningState caches cluster facts render needs on every call.
type hardeningState struct {
	versionOnce  sync.Once
	preStopSleep bool
	// priorityReady is set once ensurePriorityClasses applied the classes;
	// pods only reference them after that (a missing class rejects pods).
	priorityReady atomic.Bool
}

// settingOr returns a setting's value, or def when unset/unreadable.
func (s *server) settingOr(key, def string) string {
	if v, err := s.st.GetSetting(key); err == nil && v != "" {
		return v
	}
	return def
}

// workloadDefaults are the requests-only CPU (milli) and memory (MiB)
// defaults; 0 means that default is off.
func (s *server) workloadDefaults() (cpu, mem int64) {
	cpu, err := parseCPUMilli(s.settingOr("default_cpu_request", defaultCPURequest))
	if err != nil {
		cpu = 50
	}
	mem, err = parseMemoryMB(s.settingOr("default_memory_request", defaultMemoryRequest))
	if err != nil {
		mem = 64
	}
	return cpu, mem
}

// supportsPreStopSleep reports whether the API server knows the native
// preStop sleep action (PodLifecycleSleepAction, beta-on since 1.30).
func (s *server) supportsPreStopSleep() bool {
	s.hardening.versionOnce.Do(func() {
		s.hardening.preStopSleep = s.kube.ServerVersionAtLeast(1, 30)
	})
	return s.hardening.preStopSleep
}

// priorityClassFor picks an app kind's priority class ("" until the classes
// exist): long-running serving kinds are apps, run-to-completion kinds are
// batch.
func (s *server) priorityClassFor(kind string) string {
	if !s.hardening.priorityReady.Load() {
		return ""
	}
	switch kind {
	case "cron", "job":
		return priorityClassBatch
	}
	return priorityClassApp
}

// hardenInput fills render.Input's hardening knobs from settings, cluster
// facts and the app's policy.
func (s *server) hardenInput(in *render.Input, a store.App) {
	pol, err := s.st.GetAppPolicy(a.ID)
	if err != nil {
		pol = store.DefaultAppPolicy(a.ID)
	}
	in.DefaultCPUMilli, in.DefaultMemoryMB = s.workloadDefaults()
	in.TCPProbe = pol.Probe != "off"
	if s.kube != nil && s.supportsPreStopSleep() {
		in.PreStopSleep = preStopSleepSeconds
	}
	in.ProgressDeadline = int32(pol.RolloutTimeout)
	in.RevisionHistory = revisionHistoryLimit
	in.Spread = true
	in.PriorityClass = s.priorityClassFor(a.Kind)
	in.Security = pol.Security
}

// ensurePriorityClasses applies luncur's PriorityClasses at boot; on
// success new renders start referencing them.
func (s *server) ensurePriorityClasses(ctx context.Context) {
	if s.kube == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.kube.Apply(ctx, "", render.PriorityClasses()); err != nil {
		log.Printf("ensure priority classes (pods render without them): %v", err)
		return
	}
	s.hardening.priorityReady.Store(true)
}

// gpuPendingGrace is how long a Pending GPU pod counts as busy for the
// rented-VM idle destroy (see gpucloud.go).
func (s *server) gpuPendingGrace() time.Duration {
	n, err := strconv.Atoi(s.settingOr("gpu_pending_grace_minutes", "30"))
	if err != nil || n < 1 {
		n = 30
	}
	return time.Duration(n) * time.Minute
}

// withPriority stamps priorityClassName onto a rendered workload's pod
// template (Job, Deployment, StatefulSet, CronJob) — for objects rendered
// outside render.Render (builds, pipeline image steps, addons). No class
// (classes not ready) or a non-workload object returns o unchanged.
func withPriority(o render.Object, class string) render.Object {
	if class == "" {
		return o
	}
	var path []string
	switch o.Kind {
	case "Job", "Deployment", "StatefulSet":
		path = []string{"spec", "template", "spec", "priorityClassName"}
	case "CronJob":
		path = []string{"spec", "jobTemplate", "spec", "template", "spec", "priorityClassName"}
	default:
		return o
	}
	var u map[string]any
	if err := json.Unmarshal(o.JSON, &u); err != nil {
		return o
	}
	if err := unstructured.SetNestedField(u, class, path...); err != nil {
		return o
	}
	b, err := json.Marshal(u)
	if err != nil {
		return o
	}
	return render.Object{Kind: o.Kind, JSON: b}
}

// batchPriority is the class for luncur-run batch work ("" until ready).
func (s *server) batchPriority() string {
	if !s.hardening.priorityReady.Load() {
		return ""
	}
	return priorityClassBatch
}

// systemPriority is the class for addons ("" until ready).
func (s *server) systemPriority() string {
	if !s.hardening.priorityReady.Load() {
		return ""
	}
	return priorityClassSystem
}
