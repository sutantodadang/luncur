package kube

import (
	"strings"
	"testing"
	"time"
)

func TestJudge(t *testing.T) {
	now := time.Now()
	started := now.Add(-10 * time.Second)
	old := now.Add(-2 * time.Minute)
	pending := Rollout{Want: 2, Updated: 1, Available: 1, Replicas: 3, Observed: true}
	with := func(p RolloutPod) Rollout { r := pending; r.Pods = []RolloutPod{p}; return r }
	cases := []struct {
		name    string
		r       Rollout
		started time.Time
		done    bool
		reason  string // prefix; "" = still pending
	}{
		{"done", Rollout{Want: 2, Updated: 2, Available: 2, Replicas: 2, Observed: true}, started, true, ""},
		{"old pods remain", Rollout{Want: 2, Updated: 2, Available: 2, Replicas: 3, Observed: true}, started, false, ""},
		{"stale generation", Rollout{Want: 1, Updated: 1, Available: 1, Replicas: 1}, started, false, ""},
		{"crash loop", with(RolloutPod{Waiting: "CrashLoopBackOff", LastTerminated: "Error", Restarts: 2}), started, false, "crash-looping: Error"},
		{"restarts", with(RolloutPod{Restarts: 3}), started, false, "crash-looping: container exited"},
		{"oom", with(RolloutPod{LastTerminated: "OOMKilled", MemoryLimit: "256Mi"}), started, false, "OOM-killed: 256Mi limit hit"},
		{"config", with(RolloutPod{Waiting: "CreateContainerConfigError", WaitingMessage: "secret \"x\" not found"}), started, false, "bad container config: secret"},
		{"pull within grace", with(RolloutPod{Waiting: "ImagePullBackOff", Created: now.Add(-10 * time.Second)}), started, false, ""},
		{"pull after grace", with(RolloutPod{Waiting: "ErrImagePull", WaitingMessage: "not found", Created: old}), started, false, "image pull failed: not found"},
		{"ready pod ignored", with(RolloutPod{Ready: true, Restarts: 5}), started, false, ""},
		{"timeout", pending, now.Add(-6 * time.Minute), false, "rollout timed out after 5m0s (1/2"},
		{"unschedulable at timeout", with(RolloutPod{Unschedulable: "0/1 nodes: insufficient cpu"}), now.Add(-6 * time.Minute), false, "unschedulable: 0/1 nodes"},
		{"deadline condition", Rollout{Want: 1, Observed: true, DeadlineExceeded: true}, started, false, "rollout timed out"},
	}
	for _, c := range cases {
		v := Judge(c.r, now, c.started, 5*time.Minute)
		if v.Done != c.done {
			t.Errorf("%s: done = %v", c.name, v.Done)
		}
		if c.reason == "" && v.Failed {
			t.Errorf("%s: failed early: %s", c.name, v.Reason)
		}
		if c.reason != "" && (!v.Failed || !strings.HasPrefix(v.Reason, c.reason)) {
			t.Errorf("%s: verdict = %+v, want reason %q", c.name, v, c.reason)
		}
	}
}
