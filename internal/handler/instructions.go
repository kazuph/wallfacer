package handler

import (
	"encoding/json"
	"net/http"
	"os"

	"changkun.de/wallfacer/internal/instructions"
	"changkun.de/wallfacer/internal/logger"
)

// GetInstructions returns the current workspace CLAUDE.md content.
func (h *Handler) GetInstructions(w http.ResponseWriter, r *http.Request) {
	path := instructions.FilePath(h.configDir, h.workspaces)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]string{"content": ""})
			return
		}
		logger.Handler.Error("read instructions", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": string(content)})
}

// maxInstructionsSize is the body limit for CLAUDE.md updates (512 KB).
const maxInstructionsSize = 512 << 10

// UpdateInstructions replaces the workspace CLAUDE.md with the provided content.
func (h *Handler) UpdateInstructions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInstructionsSize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	path := instructions.FilePath(h.configDir, h.workspaces)
	if err := os.WriteFile(path, []byte(req.Content), 0600); err != nil {
		logger.Handler.Error("write instructions", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ReinitInstructions rebuilds the workspace CLAUDE.md from defaults and repo files.
func (h *Handler) ReinitInstructions(w http.ResponseWriter, r *http.Request) {
	path, err := instructions.Reinit(h.configDir, h.workspaces)
	if err != nil {
		logger.Handler.Error("reinit instructions", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		logger.Handler.Error("read reinited instructions", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": string(content)})
}
