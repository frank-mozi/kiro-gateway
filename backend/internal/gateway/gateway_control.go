package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/mydisha/keirouter/backend/internal/store"
)

const modelAPIEnabledKey = "gateway.model_api_enabled"

func (s *Server) modelAPIEnabled(ctx context.Context) (bool, error) {
	// Older installations have no setting and retain their existing behavior.
	if s.settings == nil {
		return true, nil
	}
	value, err := s.settings.Get(ctx, modelAPIEnabledKey)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid persisted model API state")
	}
}

func (s *Server) adminGatewayControl(w http.ResponseWriter, r *http.Request) {
	enabled, err := s.modelAPIEnabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, sanitizeError(s.log, err, "cannot read gateway state"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     enabled,
		"version":     s.versionString(),
		"listen_addr": s.cfg.Addr(),
		"base_url":    s.publicBaseURL(r),
	})
}

func (s *Server) adminSetGatewayControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled must be a boolean")
		return
	}
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway settings store unavailable")
		return
	}
	if err := s.settings.Set(r.Context(), modelAPIEnabledKey, strconv.FormatBool(*body.Enabled)); err != nil {
		writeError(w, http.StatusInternalServerError, sanitizeError(s.log, err, "cannot save gateway state"))
		return
	}
	s.consoleLog.Log("INFO", "Model API enabled="+strconv.FormatBool(*body.Enabled),
		"Applies to new model requests; management, discovery and in-flight requests remain available.")
	s.adminGatewayControl(w, r)
}

func (s *Server) requireModelAPIEnabled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read-only discovery, usage and job polling remain available while paused.
		if r.Method == http.MethodPost {
			enabled, err := s.modelAPIEnabled(r.Context())
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, sanitizeError(s.log, err, "cannot read gateway state"))
				return
			}
			if !enabled {
				writeError(w, http.StatusServiceUnavailable, "model API is paused; resume it from the management console")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
