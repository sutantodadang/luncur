package server

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// logBounds parses the optional ?tail= (positive line count) and ?since=
// (Go duration, e.g. "15m", "2h") query params. Absent params return zero,
// which streams unbounded — the behavior before these params existed.
func logBounds(r *http.Request) (tail, since int64, err error) {
	if v := r.URL.Query().Get("tail"); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, fmt.Errorf("tail must be a positive integer")
		}
		tail = n
	}
	if v := r.URL.Query().Get("since"); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			return 0, 0, fmt.Errorf("since must be a positive duration like 15m or 2h")
		}
		since = int64(d.Seconds())
		if since < 1 {
			since = 1
		}
	}
	return tail, since, nil
}

// handleRuntimeLogs streams the app's pod logs as SSE, each line prefixed
// with its pod name. Follow mode holds the kube streams open.
func (s *server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	follow := r.URL.Query().Get("follow") == "1"
	tail, since, err := logBounds(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	pods, err := s.kube.AppPods(r.Context(), env.Namespace, a.Name)
	if err != nil {
		log.Printf("list app pods: %v", err)
		writeError(w, http.StatusBadGateway, "kube_error", "could not list pods")
		return
	}
	if len(pods) == 0 {
		writeError(w, http.StatusNotFound, "no_pods", "app has no running pods")
		return
	}

	fl, ok := sseStart(w)
	if !ok {
		return
	}

	lines := make(chan string, 64)
	// send parks on the channel OR the request context, never just the
	// channel: once the client disconnects the main loop stops draining,
	// and an unconditional send would leak the producer goroutine (and
	// its kube log stream) forever.
	send := func(line string) bool {
		select {
		case lines <- line:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			rc, err := s.kube.PodLogStream(r.Context(), env.Namespace, pod, follow, tail, since)
			if err != nil {
				send("[" + pod + "] error: " + err.Error())
				return
			}
			defer rc.Close()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				if !send("[" + pod + "] " + sc.Text()) {
					return
				}
			}
		}(pod)
	}
	go func() { wg.Wait(); close(lines) }()

	for {
		select {
		case line, more := <-lines:
			if !more {
				sseEnd(w, fl, "eof")
				return
			}
			sseData(w, fl, line)
		case <-r.Context().Done():
			return
		}
	}
}
