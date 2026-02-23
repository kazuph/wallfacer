package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"changkun.de/wallfacer/internal/store"
	"github.com/google/uuid"
)

// SSE connection limiter.
var (
	sseConnections int64
	maxSSEConns    int64 = 100
)

// acquireSSESlot increments the SSE connection counter and returns true if the
// slot was acquired. If the limit is exceeded, it writes a 503 error and returns false.
func acquireSSESlot(w http.ResponseWriter) bool {
	if atomic.AddInt64(&sseConnections, 1) > maxSSEConns {
		atomic.AddInt64(&sseConnections, -1)
		http.Error(w, "too many SSE connections", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// releaseSSESlot decrements the SSE connection counter.
func releaseSSESlot() {
	atomic.AddInt64(&sseConnections, -1)
}

// StreamTasks streams the task list as SSE, pushing an update on every state change.
func (h *Handler) StreamTasks(w http.ResponseWriter, r *http.Request) {
	if !acquireSSESlot(w) {
		return
	}
	defer releaseSSESlot()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	includeArchived := r.URL.Query().Get("include_archived") == "true"

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	subID, ch := h.store.Subscribe()
	defer h.store.Unsubscribe(subID)

	send := func() bool {
		tasks, err := h.store.ListTasks(r.Context(), includeArchived)
		if err != nil {
			return false
		}
		if tasks == nil {
			tasks = []store.Task{}
		}
		data, err := json.Marshal(tasks)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		}
	}
}

// StreamLogs serves logs for a task. For in-progress tasks with a live.log
// file, it tails the file in real-time. For completed tasks, it serves
// the saved turn outputs.
func (h *Handler) StreamLogs(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if !acquireSSESlot(w) {
		return
	}
	defer releaseSSESlot()

	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	// For in-progress/committing tasks, try to tail the live log file.
	if task.Status == "in_progress" || task.Status == "committing" {
		liveLogPath := h.store.LiveLogPath(id)
		if _, statErr := os.Stat(liveLogPath); statErr == nil {
			h.tailLiveLog(w, r, liveLogPath)
			return
		}
	}

	// Fall back to stored turn outputs.
	h.serveStoredLogs(w, r, id)
}

// tailLiveLog streams a live log file to the HTTP response, polling for
// new content until the client disconnects or the file is removed.
func (h *Handler) tailLiveLog(w http.ResponseWriter, r *http.Request, path string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	buf := make([]byte, 4096)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// Check if file still exists (removed when exec completes).
			if _, statErr := os.Stat(path); statErr != nil {
				return
			}
			for {
				n, readErr := f.Read(buf)
				if n > 0 {
					if _, writeErr := w.Write(buf[:n]); writeErr != nil {
						return
					}
					flusher.Flush()
				}
				if readErr == io.EOF || n == 0 {
					break
				}
				if readErr != nil {
					return
				}
			}
		case <-keepalive.C:
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// serveStoredLogs serves saved turn output for tasks no longer running.
func (h *Handler) serveStoredLogs(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	outputsDir := h.store.OutputsDir(id)
	entries, err := os.ReadDir(outputsDir)
	if err != nil {
		http.Error(w, "no logs saved for this task", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	wrote := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "turn-") {
			continue
		}
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".stderr.txt") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(outputsDir, name))
		if readErr != nil || len(strings.TrimSpace(string(content))) == 0 {
			continue
		}
		w.Write(content)
		fmt.Fprintln(w)
		wrote = true
	}
	if !wrote {
		fmt.Fprintln(w, "(no output saved for this task)")
	}
}
