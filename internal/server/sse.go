package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// sseStart switches the response into event-stream mode. Returns false
// (having written a 500) when the writer can't flush.
func sseStart(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return fl, true
}

func sseData(w http.ResponseWriter, fl http.Flusher, line string) {
	fmt.Fprintf(w, "data: %s\n\n", strings.TrimRight(line, "\r\n"))
	fl.Flush()
}

func sseEnd(w http.ResponseWriter, fl http.Flusher, msg string) {
	fmt.Fprintf(w, "event: end\ndata: %s\n\n", msg)
	fl.Flush()
}

// followFile tails path from offset, emitting complete lines as SSE data
// events. done() is polled between reads; when it reports true AND the
// file is drained, the final status is sent as the end event.
func (s *server) followFile(w http.ResponseWriter, fl http.Flusher, r *http.Request, path string, done func() (bool, string)) {
	var off int64
	var partial string
	for {
		f, err := os.Open(path)
		if err == nil {
			if _, err = f.Seek(off, io.SeekStart); err == nil {
				b, _ := io.ReadAll(f)
				off += int64(len(b))
				partial += string(b)
				for {
					line, rest, found := strings.Cut(partial, "\n")
					if !found {
						break
					}
					sseData(w, fl, line)
					partial = rest
				}
			}
			f.Close()
		}
		finished, status := done()
		if finished {
			if partial != "" {
				sseData(w, fl, partial)
			}
			sseEnd(w, fl, status)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
