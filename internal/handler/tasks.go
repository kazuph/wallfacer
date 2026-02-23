package handler

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"changkun.de/wallfacer/internal/logger"
	"changkun.de/wallfacer/internal/store"
	"github.com/google/uuid"
)

// validStatuses defines the set of allowed task status values.
var validStatuses = map[string]bool{
	"backlog":     true,
	"in_progress": true,
	"done":        true,
	"waiting":     true,
	"failed":      true,
	"cancelled":   true,
	"committing":  true,
}

// validOutputFilename matches expected turn output filenames.
var validOutputFilename = regexp.MustCompile(`^turn-\d+\.(json|stderr\.txt)$`)

// maxBodySize is the default request body limit (1 MB).
const maxBodySize = 1 << 20

// ListTasks returns all tasks, optionally including archived ones.
func (h *Handler) ListTasks(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("include_archived") == "true"
	tasks, err := h.store.ListTasks(r.Context(), includeArchived)
	if err != nil {
		logger.Handler.Error("list tasks", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []store.Task{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

// CreateTask creates a new task in backlog status.
func (h *Handler) CreateTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt         string `json:"prompt"`
		Timeout        int    `json:"timeout"`
		MountWorktrees bool   `json:"mount_worktrees"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}

	task, err := h.store.CreateTask(r.Context(), req.Prompt, req.Timeout, req.MountWorktrees)
	if err != nil {
		logger.Handler.Error("create task", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.store.InsertEvent(r.Context(), task.ID, store.EventTypeStateChange, map[string]string{
		"to": "backlog",
	})

	go h.runner.GenerateTitle(task.ID, task.Prompt)

	writeJSON(w, http.StatusCreated, task)
}

// UpdateTask handles PATCH requests: status transitions, position, prompt, etc.
func (h *Handler) UpdateTask(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var req struct {
		Status         *string `json:"status"`
		Position       *int    `json:"position"`
		Prompt         *string `json:"prompt"`
		Timeout        *int    `json:"timeout"`
		FreshStart     *bool   `json:"fresh_start"`
		MountWorktrees *bool   `json:"mount_worktrees"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	// Allow editing prompt, timeout, fresh_start, and mount_worktrees for backlog tasks.
	if task.Status == "backlog" && (req.Prompt != nil || req.Timeout != nil || req.FreshStart != nil || req.MountWorktrees != nil) {
		if err := h.store.UpdateTaskBacklog(r.Context(), id, req.Prompt, req.Timeout, req.FreshStart, req.MountWorktrees); err != nil {
			logger.Handler.Error("update backlog", "task", id, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if req.Position != nil {
		if err := h.store.UpdateTaskPosition(r.Context(), id, *req.Position); err != nil {
			logger.Handler.Error("update position", "task", id, "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	if req.Status != nil {
		if !validStatuses[*req.Status] {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		oldStatus := task.Status
		newStatus := *req.Status

		// Handle retry: done/failed/waiting/cancelled → backlog
		if newStatus == "backlog" && (oldStatus == "done" || oldStatus == "failed" || oldStatus == "cancelled" || oldStatus == "waiting") {
			// Clean up any existing worktrees before resetting.
			if len(task.WorktreePaths) > 0 {
				h.runner.CleanupWorktrees(id, task.WorktreePaths, task.BranchName)
			}
			newPrompt := task.Prompt
			if req.Prompt != nil {
				newPrompt = *req.Prompt
			}
			// Default to resuming the previous session; the client can opt out by sending fresh_start=true.
			freshStart := false
			if req.FreshStart != nil {
				freshStart = *req.FreshStart
			}
			if err := h.store.ResetTaskForRetry(r.Context(), id, newPrompt, freshStart); err != nil {
				logger.Handler.Error("reset for retry", "task", id, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			h.store.InsertEvent(r.Context(), id, store.EventTypeStateChange, map[string]string{
				"from": oldStatus,
				"to":   "backlog",
			})
		} else {
			if err := h.store.UpdateTaskStatus(r.Context(), id, newStatus); err != nil {
				logger.Handler.Error("update status", "task", id, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			h.store.InsertEvent(r.Context(), id, store.EventTypeStateChange, map[string]string{
				"from": oldStatus,
				"to":   newStatus,
			})

			if newStatus == "in_progress" && oldStatus == "backlog" {
				sessionID := ""
				if !task.FreshStart && task.SessionID != nil {
					sessionID = *task.SessionID
				}
				go h.runner.Run(id, task.Prompt, sessionID, false)
			}
		}
	}

	updated, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		logger.Handler.Error("get updated task", "task", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// DeleteTask removes a task and its data.
func (h *Handler) DeleteTask(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if task, err := h.store.GetTask(r.Context(), id); err == nil && len(task.WorktreePaths) > 0 {
		h.runner.CleanupWorktrees(id, task.WorktreePaths, task.BranchName)
	}
	if err := h.store.DeleteTask(r.Context(), id); err != nil {
		logger.Handler.Error("delete task", "task", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetEvents returns the event timeline for a task.
func (h *Handler) GetEvents(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	events, err := h.store.GetEvents(r.Context(), id)
	if err != nil {
		logger.Handler.Error("get events", "task", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []store.TaskEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

// ServeOutput serves a raw turn output file for a task.
func (h *Handler) ServeOutput(w http.ResponseWriter, r *http.Request, id uuid.UUID, filename string) {
	// Strict whitelist: only allow expected turn output filenames.
	if !validOutputFilename.MatchString(filename) {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	baseDir := h.store.OutputsDir(id)
	fullPath := filepath.Join(baseDir, filename)

	// Defense-in-depth: verify the resolved path stays within the outputs directory.
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(baseDir)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(fullPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if strings.HasSuffix(filename, ".json") {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	http.ServeFile(w, r, fullPath)
}

// GenerateMissingTitles triggers background title generation for untitled tasks.
func (h *Handler) GenerateMissingTitles(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}

	tasks, err := h.store.ListTasks(r.Context(), true)
	if err != nil {
		logger.Handler.Error("list tasks for title gen", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var untitled []store.Task
	for _, t := range tasks {
		if t.Title == "" {
			untitled = append(untitled, t)
		}
	}

	total := len(untitled)
	if limit > 0 && len(untitled) > limit {
		untitled = untitled[:limit]
	}

	taskIDs := make([]string, len(untitled))
	for i, t := range untitled {
		taskIDs[i] = t.ID.String()
		go h.runner.GenerateTitle(t.ID, t.Prompt)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"queued":              len(untitled),
		"total_without_title": total,
		"task_ids":            taskIDs,
	})
}

// --- Artifact discovery and serving ---

// ArtifactInfo describes a file discovered in a task's worktree.
type ArtifactInfo struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// artifactExtensions maps file extensions to artifact type categories.
var artifactExtensions = map[string]string{
	".html": "html", ".htm": "html",
	".svg":     "svg",
	".png":     "image",
	".jpg":     "image",
	".jpeg":    "image",
	".gif":     "image",
	".webp":    "image",
	".mp4":     "video",
	".webm":    "video",
	".md":      "markdown",
	".mermaid": "mermaid",
	".mmd":     "mermaid",
}

// blockedDirNames lists directory names that should never be traversed.
var blockedDirNames = map[string]bool{
	".git": true, "node_modules": true,
}

// blockedFileNames lists filenames that should never be served.
var blockedFileNames = map[string]bool{
	".env": true, ".env.local": true, ".env.production": true,
}

// ListArtifacts returns files created/modified by the task in its worktrees.
func (h *Handler) ListArtifacts(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	var artifacts []ArtifactInfo
	for repoPath, wtPath := range task.WorktreePaths {
		wsKey := filepath.Base(repoPath)
		filepath.WalkDir(wtPath, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if blockedDirNames[name] || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if blockedFileNames[name] {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(name))
			artifactType, ok := artifactExtensions[ext]
			if !ok {
				return nil
			}
			relPath, err := filepath.Rel(wtPath, path)
			if err != nil {
				return nil
			}
			var size int64
			if info, infoErr := d.Info(); infoErr == nil {
				size = info.Size()
			}
			artifacts = append(artifacts, ArtifactInfo{
				Path: wsKey + "/" + relPath,
				Name: name,
				Type: artifactType,
				Size: size,
			})
			return nil
		})
	}

	if artifacts == nil {
		artifacts = []ArtifactInfo{}
	}
	writeJSON(w, http.StatusOK, artifacts)
}

// ServeArtifact serves a specific file from a task's worktree.
// Path format: {workspace_basename}/{relative_path}
func (h *Handler) ServeArtifact(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	reqPath := r.PathValue("path")
	if reqPath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	// Split into workspace key and relative path.
	slashIdx := strings.IndexByte(reqPath, '/')
	if slashIdx < 0 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	wsKey := reqPath[:slashIdx]
	relPath := reqPath[slashIdx+1:]
	if relPath == "" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Find the matching worktree.
	var fullPath string
	for repoPath, wtPath := range task.WorktreePaths {
		if filepath.Base(repoPath) == wsKey {
			candidate := filepath.Join(wtPath, relPath)
			resolved := filepath.Clean(candidate)
			cleanWt := filepath.Clean(wtPath)
			// Path traversal defense: resolved path must be within the worktree.
			if !strings.HasPrefix(resolved, cleanWt+string(filepath.Separator)) {
				http.Error(w, "invalid path", http.StatusBadRequest)
				return
			}
			fullPath = resolved
			break
		}
	}

	if fullPath == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Block sensitive path segments.
	for _, seg := range strings.Split(fullPath, string(filepath.Separator)) {
		if blockedDirNames[seg] || blockedFileNames[seg] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	if _, err := os.Stat(fullPath); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Allow iframe embedding for same-origin (overrides DENY set by middleware).
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	http.ServeFile(w, r, fullPath)
}
