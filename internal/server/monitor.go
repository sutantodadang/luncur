package server

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/kube"
)

// monitorWindow is how many samples each ring keeps; at monitorInterval
// spacing that is a ~30 minute window.
const (
	monitorInterval = 15 * time.Second
	monitorWindow   = 120
)

// crashCooldown suppresses repeat app_unhealthy notifications while an app
// keeps crash-looping; one alert per cooldown window per app.
const crashCooldown = 30 * time.Minute

// metricSample is one point on a live chart.
type metricSample struct {
	At        time.Time
	CPUMilli  int64
	MemoryMiB int64
}

// metricRing is a fixed-size append-only ring; zero value ready to use.
type metricRing struct {
	buf [monitorWindow]metricSample
	n   int // total appended, min(n, monitorWindow) valid
}

func (r *metricRing) add(s metricSample) {
	r.buf[r.n%monitorWindow] = s
	r.n++
}

// snapshot returns a copy of the ring's valid samples, oldest-first.
func (r *metricRing) snapshot() []metricSample {
	count := r.n
	if count > monitorWindow {
		count = monitorWindow
	}
	out := make([]metricSample, count)
	start := r.n - count
	for i := 0; i < count; i++ {
		out[i] = r.buf[(start+i)%monitorWindow]
	}
	return out
}

// monitor holds live metric history in memory only — it resets on restart
// (charts refill within a couple of samples) and is capped at
// monitorWindow points per app/node, so no persistence or pruning.
type monitor struct {
	mu    sync.Mutex
	apps  map[string]*metricRing // key "<namespace>/<app>"
	nodes map[string]*metricRing // key node name; CPUMilli/MemoryMiB = usage

	// restarts/lastAlert track crash-loop state per app key ("<namespace>/<app>"),
	// guarded by mu alongside the metric rings.
	restarts  map[string]int
	lastAlert map[string]time.Time
}

func newMonitor() *monitor {
	return &monitor{
		apps:      make(map[string]*metricRing),
		nodes:     make(map[string]*metricRing),
		restarts:  make(map[string]int),
		lastAlert: make(map[string]time.Time),
	}
}

// crashLoopCheck records the current restart count for key and reports
// whether an app_unhealthy alert should fire: at least 3 more restarts since
// the previous sample, and no alert already sent within cooldown. The first
// sample ever seen for a key just establishes the baseline — nothing to
// compare against yet, so it never alerts.
func (m *monitor) crashLoopCheck(now time.Time, key string, count int, cooldown time.Duration) (delta int, alert bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, had := m.restarts[key]
	delta = count - prev
	m.restarts[key] = count
	if !had || delta < 3 {
		return delta, false
	}
	if last, ok := m.lastAlert[key]; ok && now.Sub(last) < cooldown {
		return delta, false
	}
	m.lastAlert[key] = now
	return delta, true
}

// record adds one sample per app/node this tick reported, and zero-fills
// any ring that already exists but is absent from this tick (app scaled
// down/crashed, or a node whose metrics aren't available) so its chart
// drops to 0 instead of freezing on its last value.
func (m *monitor) record(at time.Time, apps map[string]kube.AppMetrics, nodes []kube.NodeInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seenApps := make(map[string]bool, len(apps))
	for key, am := range apps {
		seenApps[key] = true
		r, ok := m.apps[key]
		if !ok {
			r = &metricRing{}
			m.apps[key] = r
		}
		r.add(metricSample{At: at, CPUMilli: am.CPUMilli, MemoryMiB: am.MemoryMiB})
	}
	for key, r := range m.apps {
		if !seenApps[key] {
			r.add(metricSample{At: at})
		}
	}

	seenNodes := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if !n.MetricsOK {
			continue
		}
		seenNodes[n.Name] = true
		r, ok := m.nodes[n.Name]
		if !ok {
			r = &metricRing{}
			m.nodes[n.Name] = r
		}
		r.add(metricSample{At: at, CPUMilli: n.CPUMilli, MemoryMiB: n.MemMiB})
	}
	for name, r := range m.nodes {
		if !seenNodes[name] {
			r.add(metricSample{At: at})
		}
	}
}

func (m *monitor) appSamples(key string) []metricSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.apps[key]
	if !ok {
		return nil
	}
	return r.snapshot()
}

func (m *monitor) nodeSamples(name string) []metricSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.nodes[name]
	if !ok {
		return nil
	}
	return r.snapshot()
}

// nodeKeys returns the names of every node currently tracked, sorted —
// used by handleUINodeCharts to render one chart per known node.
func (m *monitor) nodeKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.nodes))
	for k := range m.nodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sampleMetrics takes one monitor tick; separated from StartMonitor so
// tests can drive it with a fake clock.
func (s *server) sampleMetrics(ctx context.Context) {
	if s.kube == nil {
		return
	}
	apps, ok := s.kube.ClusterPodUsage(ctx)
	if !ok {
		apps = nil
	}
	nodes, err := s.kube.ListNodes(ctx)
	if err != nil {
		nodes = nil
	}
	now := s.nowFn()
	s.mon.record(now, apps, nodes)
	s.checkCrashLoops(ctx, now)
}

// checkCrashLoops polls every known app's restart count and fires
// app_unhealthy (via s.notify, fire-and-forget) when it jumps by >=3 since
// the previous sample, subject to crashCooldown per app.
func (s *server) checkCrashLoops(ctx context.Context, now time.Time) {
	projects, err := s.st.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		apps, err := s.st.ListApps(p.ID)
		if err != nil {
			continue
		}
		for _, a := range apps {
			count, err := s.kube.RestartCount(ctx, p.Namespace, a.Name)
			if err != nil {
				continue
			}
			key := p.Namespace + "/" + a.Name
			if delta, alert := s.mon.crashLoopCheck(now, key, count, crashCooldown); alert {
				s.notify(notifyEvent{
					Event:   "app_unhealthy",
					Project: p.Name,
					App:     a.Name,
					Err:     fmt.Sprintf("%d container restarts in the last %s", delta, monitorInterval*2),
				})
			}
		}
	}
}

// StartMonitor runs the metrics sampler until ctx ends. No-op without kube.
func (s *server) StartMonitor(ctx context.Context) {
	if s.kube == nil {
		return
	}
	t := time.NewTicker(monitorInterval)
	defer t.Stop()
	s.sampleMetrics(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampleMetrics(ctx)
		}
	}
}
