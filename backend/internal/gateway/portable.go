package gateway

import (
	"net/http"

	"github.com/mydisha/keirouter/backend/internal/portable"
)

func (s *Server) adminPortableStatus(w http.ResponseWriter, r *http.Request) {
	if s.portable == nil {
		writeJSON(w, http.StatusOK, portable.State{Current: s.versionString(), Phase: "unmanaged"})
		return
	}
	s.portableCall(w, r, http.MethodGet, "/status", nil)
}

func (s *Server) portableCall(w http.ResponseWriter, r *http.Request, method, path string, body any) {
	if s.portable == nil {
		writeError(w, 409, "start this installation with 'kiro-gateway portable' to enable managed updates")
		return
	}
	state, err := s.portable.Call(r.Context(), method, path, body)
	if err != nil {
		s.log.Warn("portable operation failed", "operation", path, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) adminPortableSource(w http.ResponseWriter, r *http.Request) {
	var source portable.Source
	if !decodeJSON(w, r, &source) {
		return
	}
	s.portableCall(w, r, http.MethodPost, "/source", source)
}

func (s *Server) adminPortableCheck(w http.ResponseWriter, r *http.Request) {
	s.portableCall(w, r, http.MethodPost, "/check", nil)
}

func (s *Server) adminPortableInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	s.portableCall(w, r, http.MethodPost, "/install", body)
}
