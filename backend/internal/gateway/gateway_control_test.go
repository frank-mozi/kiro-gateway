package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mydisha/keirouter/backend/internal/config"
	"github.com/mydisha/keirouter/backend/internal/consolelog"
)

func TestGatewayControlPersistenceAndValidation(t *testing.T) {
	s, db := newBulkTestServer(t)
	s.settings = db.Settings()
	s.consoleLog = consolelog.New()
	s.cfg = config.Default()
	enabled, err := s.modelAPIEnabled(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)

	for _, invalid := range []string{`{}`, `{"enabled":null}`, `{"enabled":"false"}`} {
		rec := httptest.NewRecorder()
		s.adminSetGatewayControl(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(invalid)))
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
	for _, value := range []string{"false", "true"} {
		rec := httptest.NewRecorder()
		s.adminSetGatewayControl(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"enabled":`+value+`}`)))
		require.Equal(t, http.StatusOK, rec.Code)
		// A new server reads persisted state rather than a process-local toggle.
		next := New(Deps{Config: config.Default(), Settings: db.Settings()})
		state, err := next.modelAPIEnabled(context.Background())
		require.NoError(t, err)
		require.Equal(t, value == "true", state)
	}
	require.NoError(t, db.Settings().Set(context.Background(), modelAPIEnabledKey, "invalid"))
	_, err = s.modelAPIEnabled(context.Background())
	require.Error(t, err)
	rec := httptest.NewRecorder()
	s.requireModelAPIEnabled(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid state must not forward a model request")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestGatewayControlPausedRoutes(t *testing.T) {
	s, key := newModelDiscoveryTestGateway(t, nil)
	require.NoError(t, s.settings.Set(context.Background(), modelAPIEnabledKey, "false"))
	for _, path := range []string{
		"/v1/chat/completions", "/v1/messages", "/v1/responses",
		"/v1/messages/count_tokens", "/v1/embeddings",
		"/v1/images/generations", "/v1/images/understanding", "/v1/videos/generations",
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/search", "/v1/web/fetch",
		"/v1beta/models/test:generateContent", "/v1beta/models/test:streamGenerateContent", "/v1/v1/responses",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+key)
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "paused")
		})
	}
	for _, path := range []string{"/v1/models", "/v1/models/llm", "/healthz", "/v1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code, "pausing must not bypass authentication")
}

func TestGatewayControlDoesNotAbortInflightRequests(t *testing.T) {
	s, db := newBulkTestServer(t)
	s.settings = db.Settings()
	entered := make(chan struct{})
	release := make(chan struct{}, 1)
	defer close(release)
	done := make(chan struct{})
	handler := s.requireModelAPIEnabled(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	first := httptest.NewRecorder()
	go func() {
		handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		close(done)
	}()
	<-entered
	require.NoError(t, s.settings.Set(context.Background(), modelAPIEnabledKey, "false"))
	next := httptest.NewRecorder()
	handler.ServeHTTP(next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	require.Equal(t, http.StatusServiceUnavailable, next.Code)
	release <- struct{}{}
	<-done
	require.Equal(t, http.StatusOK, first.Code)
}

func TestGatewayControlStorageFailure(t *testing.T) {
	s, db := newBulkTestServer(t)
	s.settings = db.Settings()
	require.NoError(t, db.Close())
	rec := httptest.NewRecorder()
	s.adminSetGatewayControl(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"enabled":false}`)))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	rec = httptest.NewRecorder()
	s.adminGatewayControl(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestGatewayControlRequiresDashboardSession(t *testing.T) {
	srv := newAuthGateway(t, config.Default())
	resp, err := http.Post(srv.URL+"/api/gateway/control", "application/json", strings.NewReader(`{"enabled":false}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	login, err := http.Post(srv.URL+"/api/auth/login", "application/json", strings.NewReader(`{"password":"keirouter"}`))
	require.NoError(t, err)
	defer login.Body.Close()
	cookie := findCookie(login.Cookies(), sessionCookie)
	require.NotNil(t, cookie)
	for _, value := range []bool{false, true} {
		payload := `{"enabled":false}`
		if value {
			payload = `{"enabled":true}`
		}
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/gateway/control", strings.NewReader(payload))
		require.NoError(t, err)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var body struct {
			Enabled bool `json:"enabled"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		resp.Body.Close()
		require.Equal(t, value, body.Enabled)
	}
}
