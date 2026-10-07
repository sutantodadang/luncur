package client

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A followed log stream must outlive the plain request timeout (30s in New;
// shrunk here): stream() can't use the whole-exchange-timeout client.
func TestFollowStreamOutlivesClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "data: line %d\n\n", i)
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprint(w, "event: end\ndata: live\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "tok")
	c.http.Timeout = 300 * time.Millisecond // stands in for the real 30s
	if err := c.stream("/logs?follow=1", io.Discard); err != nil {
		t.Fatalf("healthy 1s log stream aborted by the request timeout: %v", err)
	}
}
