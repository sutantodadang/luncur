package server

import (
	"bufio"
	"log"
	"net/http"
	"sync"

	"github.com/sutantodadang/luncur/internal/store"
)

// handleRuntimeLogs streams the app's pod logs as SSE, each line prefixed
// with its pod name. Follow mode holds the kube streams open.
func (s *server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	follow := r.URL.Query().Get("follow") == "1"
	pods, err := s.kube.AppPods(r.Context(), p.Namespace, a.Name)
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
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			rc, err := s.kube.PodLogStream(r.Context(), p.Namespace, pod, follow)
			if err != nil {
				lines <- "[" + pod + "] error: " + err.Error()
				return
			}
			defer rc.Close()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				lines <- "[" + pod + "] " + sc.Text()
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
