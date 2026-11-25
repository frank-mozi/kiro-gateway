package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mydisha/keirouter/backend/internal/auth"
	"github.com/mydisha/keirouter/backend/internal/config"
	"github.com/mydisha/keirouter/backend/internal/portable"
	"github.com/mydisha/keirouter/backend/internal/store"
)

func TestLocalConsoleRouteRegistered(t *testing.T) {
	server := newAuthGateway(t, config.Default())
	response, err := http.Post(server.URL+"/api/auth/local-session", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusForbidden, response.StatusCode, "ordinary mode must register but deny the portable entry point, not return 404")
}

func TestLocalConsoleSession(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 20311
	cfg.Security.BindLoopbackOnly = true
	db, err := store.Open(context.Background(), config.DatabaseConfig{Driver: "sqlite", DSN: ":memory:"}, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.Migrate(context.Background()))
	service := auth.New(db.Settings(), "", 0)
	_, err = service.EnsureDefaults(context.Background())
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		change  func(*Server, *http.Request)
		allowed bool
	}{
		{"same origin portable", func(*Server, *http.Request) {}, true},
		{"not portable", func(s *Server, _ *http.Request) { s.portable = nil }, false},
		{"wildcard bind", func(s *Server, _ *http.Request) { s.cfg.Server.Host = "0.0.0.0" }, false},
		{"remote peer", func(_ *Server, r *http.Request) { r.RemoteAddr = "192.0.2.1:2345" }, false},
		{"untrusted origin", func(_ *Server, r *http.Request) { r.Header.Set("Origin", "https://example.org") }, false},
		{"different port", func(_ *Server, r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1234") }, false},
		{"missing origin", func(_ *Server, r *http.Request) { r.Header.Del("Origin") }, false},
		{"opaque origin", func(_ *Server, r *http.Request) { r.Header.Set("Origin", "null") }, false},
		{"DNS rebinding host", func(_ *Server, r *http.Request) {
			r.Host = "example.org:20311"
			r.Header.Set("Origin", "http://example.org:20311")
		}, false},
		{"forwarded request", func(_ *Server, r *http.Request) { r.Header.Set("X-Forwarded-For", "192.0.2.1") }, false},
		{"cross-site metadata", func(_ *Server, r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, false},
		{"simple form", func(_ *Server, r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: cfg, auth: service, portable: &portable.Client{}}
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20311/api/auth/local-session", nil)
			r.RemoteAddr = "127.0.0.1:2345"
			r.Header.Set("Origin", "http://127.0.0.1:20311")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			tc.change(s, r)
			w := httptest.NewRecorder()
			s.handleLocalSession(w, r)
			if !tc.allowed {
				require.Equal(t, http.StatusForbidden, w.Code)
				require.Empty(t, w.Result().Cookies())
				return
			}
			require.Equal(t, http.StatusOK, w.Code)
			cookie := findCookie(w.Result().Cookies(), sessionCookie)
			require.NotNil(t, cookie)
			require.True(t, cookie.HttpOnly)
			require.True(t, service.VerifySession(cookie.Value))
			protected := s.sessionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
			req := httptest.NewRequest(http.MethodGet, "/api/gateway/control", nil)
			w = httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			require.Equal(t, 401, w.Code)
			req.AddCookie(cookie)
			w = httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			require.Equal(t, 204, w.Code)
			require.False(t, service.OnboardingComplete(context.Background()), "portable entry must not change ordinary-mode onboarding")
		})
	}
}
