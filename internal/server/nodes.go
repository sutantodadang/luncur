package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/store"
)

// drainState is one node's drain, tracked in memory (a drain is a
// minutes-long operation; a restart mid-drain leaves the node cordoned,
// which is the safe state, and drain can simply be run again).
type drainState struct {
	State    string   `json:"state"` // draining|done|failed
	Total    int      `json:"total"`
	Evicted  int      `json:"evicted"`
	Blocked  []string `json:"blocked,omitempty"`
	Error    string   `json:"error,omitempty"`
	Started  string   `json:"started_at"`
	Finished string   `json:"finished_at,omitempty"`
}

type drainTracker struct {
	mu     sync.Mutex
	byNode map[string]*drainState
}

func (t *drainTracker) get(node string) *drainState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d, ok := t.byNode[node]; ok {
		cp := *d
		return &cp
	}
	return nil
}

func (t *drainTracker) update(node string, f func(*drainState)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byNode == nil {
		t.byNode = map[string]*drainState{}
	}
	d, ok := t.byNode[node]
	if !ok {
		d = &drainState{}
		t.byNode[node] = d
	}
	f(d)
}

// nodeView is a node plus its drain status.
type nodeView struct {
	kube.NodeInfo
	Drain *drainState `json:"drain,omitempty"`
}

func (s *server) nodeViews(ctx context.Context) ([]nodeView, error) {
	nodes, err := s.kube.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]nodeView, len(nodes))
	for i, n := range nodes {
		out[i] = nodeView{NodeInfo: n, Drain: s.drains.get(n.Name)}
	}
	return out, nil
}

func (s *server) handleListNodes(w http.ResponseWriter, r *http.Request, _ store.User) {
	if !s.requireKube(w) {
		return
	}
	nodes, err := s.nodeViews(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "kube_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

var (
	errOnlyNode     = errors.New("this is the only schedulable node: draining it evicts every app and luncur itself (pass force to do it anyway)")
	errDrainRunning = errors.New("a drain of this node is already running")
	errNoSuchNode   = errors.New("no such node")
)

// startDrain validates and launches a node drain in the background.
func (s *server) startDrain(ctx context.Context, node string, force bool, timeout time.Duration) error {
	nodes, err := s.kube.ListNodes(ctx)
	if err != nil {
		return err
	}
	found, others := false, 0
	for _, n := range nodes {
		if n.Name == node {
			found = true
			continue
		}
		if n.Ready && !n.Cordoned {
			others++
		}
	}
	if !found {
		return errNoSuchNode
	}
	if others == 0 && !force {
		return errOnlyNode
	}
	if d := s.drains.get(node); d != nil && d.State == "draining" {
		return errDrainRunning
	}
	s.drains.update(node, func(d *drainState) {
		*d = drainState{State: "draining", Started: time.Now().UTC().Format(time.RFC3339)}
	})
	go func() {
		dctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
		defer cancel()
		prog, err := s.kube.Drain(dctx, node, kube.DrainOptions{
			Timeout: timeout,
			IsLast:  s.isLuncurPod,
			Progress: func(p kube.DrainProgress) {
				s.drains.update(node, func(d *drainState) { d.Total, d.Evicted, d.Blocked = p.Total, p.Evicted, p.Blocked })
			},
		})
		s.drains.update(node, func(d *drainState) {
			d.Total, d.Evicted, d.Blocked = prog.Total, prog.Evicted, prog.Blocked
			d.Finished = time.Now().UTC().Format(time.RFC3339)
			d.State = "done"
			if err != nil {
				d.State, d.Error = "failed", err.Error()
			}
		})
		if err != nil {
			log.Printf("drain %s: %v", node, err)
		}
	}()
	return nil
}

// isLuncurPod marks luncur's own server pod, evicted last by a drain.
func (s *server) isLuncurPod(p corev1.Pod) bool {
	return p.Namespace == s.systemNamespace && p.Labels["app.kubernetes.io/name"] == "luncur"
}

// handleNodeAction serves POST /v1/nodes/{name}/{cordon,uncordon,drain}.
func (s *server) handleNodeAction(action string) func(http.ResponseWriter, *http.Request, store.User) {
	return func(w http.ResponseWriter, r *http.Request, _ store.User) {
		if !s.requireKube(w) {
			return
		}
		node := r.PathValue("name")
		var req struct {
			Force   bool `json:"force"`
			Timeout int  `json:"timeout"` // seconds
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
				return
			}
		}
		if err := s.nodeAction(r.Context(), action, node, req.Force, time.Duration(req.Timeout)*time.Second); err != nil {
			writeNodeError(w, err)
			return
		}
		status := map[string]string{"cordon": "cordoned", "uncordon": "schedulable", "drain": "draining"}[action]
		writeJSON(w, http.StatusOK, map[string]any{"node": node, "status": status})
	}
}

// nodeAction is the shared core of the API and UI node actions.
func (s *server) nodeAction(ctx context.Context, action, node string, force bool, timeout time.Duration) error {
	if timeout <= 0 || timeout > time.Hour {
		timeout = 5 * time.Minute
	}
	switch action {
	case "cordon", "uncordon":
		if d := s.drains.get(node); action == "uncordon" && d != nil && d.State == "draining" {
			return errDrainRunning
		}
		return s.kube.Cordon(ctx, node, action == "cordon")
	case "drain":
		return s.startDrain(ctx, node, force, timeout)
	}
	return fmt.Errorf("unknown node action %q", action)
}

func writeNodeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSuchNode):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, errOnlyNode), errors.Is(err, errDrainRunning):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "kube_error", err.Error())
	}
}
