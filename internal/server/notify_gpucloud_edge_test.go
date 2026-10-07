package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/pipeline"
)

// A pipeline notify: step delivers whenever a notify channel is configured,
// regardless of the notify_events subscription (which omits "pipeline" by default).
func TestPipelineNotifyStepDeliversWithDefaultSettings(t *testing.T) {
	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(hook.Close)

	s := pipelineTestServer(t, recordingDyn(t), nil)
	setSealedNotifyURL(t, s, hook.URL)
	p := pipelineSeedProject(t, s.st, "ml")
	pl := pipelineSeedPipeline(t, s.st, p.ID, "pipe")
	run := pipelineSeedRun(t, s.st, pl, []pipeline.Step{{Name: "ping", Kind: "notify", Notify: "training finished"}})

	s.pipelineTick(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatalf("notify step %+v but webhook received nothing", pipelineFindStep(t, s.st, run.ID, "ping"))
	}
}

// Destroying a GPU instance goes to its own provider: a nebius VM must never
// be sent to vast.ai (it would keep billing).
func TestDestroyGPUInstanceUsesRowProvider(t *testing.T) {
	s, _ := gpuTestServer(t)
	g, err := s.st.CreateGPUInstance("nebius", "computeinstance-abc", "luncur-gpu-1", "H100", 1)
	if err != nil {
		t.Fatal(err)
	}
	err = s.destroyGPUInstance(context.Background(), g.ID)
	if err != nil && strings.Contains(err.Error(), "vast.ai") {
		t.Fatalf("destroying a nebius instance went to vast.ai: %v", err)
	}
}

// Two rents in the same second get distinct labels: the label is also the
// K3s node name and the idle loop's key.
func TestRentGPULabelsAreUnique(t *testing.T) {
	s, _ := gpuTestServer(t)
	var contract atomic.Int64
	vastFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"new_contract":` + strconv.FormatInt(contract.Add(1), 10) + `}`))
	}))
	t.Cleanup(vastFake.Close)
	s.vastBaseURL = vastFake.URL
	if err := s.storeGPUKey("k"); err != nil {
		t.Fatal(err)
	}
	a, err := s.rentGPU(context.Background(), "vastai", 101, 40, "4090", 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.rentGPU(context.Background(), "vastai", 102, 40, "4090", 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Label == b.Label {
		t.Fatalf("two rented instances share label/node name %q", a.Label)
	}
}
