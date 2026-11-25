package gateway

import (
	"net/http"

	"github.com/mydisha/keirouter/backend/internal/update"
)

// versionString returns the running build version, defaulting to "dev" when the
// gateway was constructed without an injected version (e.g. local `go run`).
func (s *Server) versionString() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

// The advanced dashboard uses the same custom release source as the manager.
// Never offer upstream KeiRouter releases as updates to this custom build.
func (s *Server) adminUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.portable == nil {
		writeJSON(w, http.StatusOK, &update.Info{Current: s.versionString(), Checked: false})
		return
	}
	method, path := http.MethodGet, "/status"
	if q := r.URL.Query().Get("refresh"); q == "1" || q == "true" {
		method, path = http.MethodPost, "/check"
	}
	state, err := s.portable.Call(r.Context(), method, path, nil)
	if err != nil {
		s.log.Warn("custom release check failed", "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, &update.Info{
		Current: state.Current, Latest: state.Latest, Changelog: state.Notes,
		UpdateAvailable: state.UpdateAvailable, Checked: state.CheckedAt != "",
	})
}
