package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

//go:embed ui
var uiFiles embed.FS

func main() {
	addr := flag.String("addr", envOrDefault("ADDR", ":8080"), "listen address")
	dataDir := flag.String("data", envOrDefault("DATA_DIR", "data"), "data directory")
	containerCmd := flag.String("container", envOrDefault("CONTAINER_CMD", "/opt/podman/bin/podman"), "container runtime command")
	sandboxImage := flag.String("image", envOrDefault("SANDBOX_IMAGE", "wallfacer:latest"), "sandbox container image")
	envFile := flag.String("env", envOrDefault("ENV_FILE", ".env"), "env file for container (Claude token)")
	noBrowser := flag.Bool("no-browser", false, "do not open browser on start")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: wallfacer [flags] [workspace ...]\n\n")
		fmt.Fprintf(os.Stderr, "Positional arguments:\n")
		fmt.Fprintf(os.Stderr, "  workspace    directories to mount in the sandbox (default: current directory)\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Positional args are workspace directories.
	workspaces := flag.Args()
	if len(workspaces) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatalf("getwd: %v", err)
		}
		workspaces = []string{cwd}
	}

	// Resolve to absolute paths and validate.
	for i, ws := range workspaces {
		abs, err := filepath.Abs(ws)
		if err != nil {
			log.Fatalf("resolve workspace %q: %v", ws, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			log.Fatalf("workspace %q: %v", abs, err)
		}
		if !info.IsDir() {
			log.Fatalf("workspace %q is not a directory", abs)
		}
		workspaces[i] = abs
	}

	store, err := NewStore(*dataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()
	log.Printf("store loaded from %s/", *dataDir)

	// Recover orphaned in_progress tasks from a previous server crash.
	recoverOrphanedTasks(store)

	runner := NewRunner(store, RunnerConfig{
		Command:      *containerCmd,
		SandboxImage: *sandboxImage,
		EnvFile:      *envFile,
		Workspaces:   strings.Join(workspaces, " "),
	})

	log.Printf("workspaces: %s", strings.Join(workspaces, ", "))

	handler := NewHandler(store, runner)

	mux := http.NewServeMux()

	// Static files (Kanban UI)
	uiFS, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(uiFS)))

	// API routes
	mux.HandleFunc("GET /api/tasks", handler.ListTasks)
	mux.HandleFunc("POST /api/tasks", handler.CreateTask)

	mux.HandleFunc("PATCH /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.UpdateTask(w, r, id)
	})

	mux.HandleFunc("DELETE /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.DeleteTask(w, r, id)
	})

	mux.HandleFunc("POST /api/tasks/{id}/feedback", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.SubmitFeedback(w, r, id)
	})

	mux.HandleFunc("GET /api/tasks/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.GetEvents(w, r, id)
	})

	mux.HandleFunc("POST /api/tasks/{id}/done", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.CompleteTask(w, r, id)
	})

	mux.HandleFunc("POST /api/tasks/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.ResumeTask(w, r, id)
	})

	mux.HandleFunc("GET /api/tasks/{id}/outputs/{filename}", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.ServeOutput(w, r, id, r.PathValue("filename"))
	})

	mux.HandleFunc("GET /api/tasks/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid task id", http.StatusBadRequest)
			return
		}
		handler.StreamLogs(w, r, id)
	})

	if !*noBrowser {
		url := "http://localhost" + *addr
		if !strings.HasPrefix(*addr, ":") {
			url = "http://" + *addr
		}
		go openBrowser(url)
	}

	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	default:
		return
	}
	exec.Command(cmd, url).Start()
}

func recoverOrphanedTasks(store *Store) {
	ctx := context.Background()
	tasks, err := store.ListTasks(ctx)
	if err != nil {
		log.Printf("recovery: list tasks: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Status != "in_progress" {
			continue
		}
		log.Printf("recovery: task %s was in_progress at startup, marking as failed", t.ID)
		store.UpdateTaskStatus(ctx, t.ID, "failed")
		store.InsertEvent(ctx, t.ID, "error", map[string]string{
			"error": "server restarted while task was in progress",
		})
		store.InsertEvent(ctx, t.ID, "state_change", map[string]string{
			"from": "in_progress", "to": "failed",
		})
	}
}
