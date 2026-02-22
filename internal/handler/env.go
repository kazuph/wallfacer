package handler

import (
	"encoding/json"
	"net/http"

	"changkun.de/wallfacer/internal/envconfig"
	"changkun.de/wallfacer/internal/logger"
)

// envConfigResponse is the JSON representation of the env config sent to the UI.
// Sensitive tokens are masked so they are never exposed in full over HTTP.
type envConfigResponse struct {
	OAuthToken string `json:"oauth_token"` // masked
	APIKey     string `json:"api_key"`     // masked
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
}

// GetEnvConfig returns the current env configuration with tokens masked.
func (h *Handler) GetEnvConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := envconfig.Parse(h.envFile)
	if err != nil {
		logger.Handler.Error("read env config", "error", err)
		http.Error(w, "failed to read configuration", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, envConfigResponse{
		OAuthToken: envconfig.MaskToken(cfg.OAuthToken),
		APIKey:     envconfig.MaskToken(cfg.APIKey),
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
	})
}

// UpdateEnvConfig writes changes to the env file.
//
// Pointer semantics per field:
//   - field absent from JSON body (null) → leave unchanged
//   - field present with a value          → update
//   - field present with ""               → clear (for non-secret fields)
//
// For the two token fields (oauth_token, api_key), an empty value is treated
// as "no change" to prevent accidental token deletion.
func (h *Handler) UpdateEnvConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OAuthToken *string `json:"oauth_token"`
		APIKey     *string `json:"api_key"`
		BaseURL    *string `json:"base_url"`
		Model      *string `json:"model"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Guard: treat empty-string tokens as "no change" to avoid accidental clears.
	if req.OAuthToken != nil && *req.OAuthToken == "" {
		req.OAuthToken = nil
	}
	if req.APIKey != nil && *req.APIKey == "" {
		req.APIKey = nil
	}

	if err := envconfig.Update(h.envFile, req.OAuthToken, req.APIKey, req.BaseURL, req.Model); err != nil {
		logger.Handler.Error("update env config", "error", err)
		http.Error(w, "failed to update configuration", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
