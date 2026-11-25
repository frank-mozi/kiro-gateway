package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mydisha/keirouter/backend/internal/auth"
	"github.com/mydisha/keirouter/backend/internal/config"
	"github.com/mydisha/keirouter/backend/internal/connectors"
	"github.com/mydisha/keirouter/backend/internal/core"
	"github.com/mydisha/keirouter/backend/internal/oauth"
)

type enterpriseTestValidator struct {
	core.Connector
	calls int
	err   error
}

func (*enterpriseTestValidator) ID() string { return "kiro" }
func (v *enterpriseTestValidator) Validate(context.Context, core.Credentials) error {
	v.calls++
	return v.err
}

func TestKiroEnterpriseRoutesRequireSession(t *testing.T) {
	server := newAuthGateway(t, config.Default())
	login, err := http.Post(server.URL+"/api/auth/login", "application/json", strings.NewReader(`{"password":"`+auth.DefaultPassword+`"}`))
	require.NoError(t, err)
	defer login.Body.Close()
	cookie := findCookie(login.Cookies(), sessionCookie)
	require.NotNil(t, cookie)
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/api/kiro/enterprise/start"},
		{"GET", "/api/kiro/enterprise/status?id=unknown"},
		{"POST", "/api/kiro/enterprise/cancel"},
		{"POST", "/api/kiro/enterprise/profile"},
	} {
		for _, signedIn := range []bool{false, true} {
			req, err := http.NewRequest(endpoint.method, server.URL+endpoint.path, strings.NewReader("{}"))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if signedIn {
				req.AddCookie(cookie)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			if !signedIn {
				require.Equal(t, 401, resp.StatusCode)
			} else if endpoint.method == "GET" {
				require.Equal(t, 404, resp.StatusCode, "unknown session must not return success")
			} else {
				require.NotEqual(t, 404, resp.StatusCode, "the route must be mounted")
				require.NotEqual(t, 401, resp.StatusCode)
			}
		}
	}
}

func TestKiroEnterpriseCredentialsUseExistingVault(t *testing.T) {
	s, db := newBulkTestServer(t)
	validator := &enterpriseTestValidator{}
	s.conns = connectors.NewRegistry(validator)
	tokens := &oauth.Tokens{
		AccessToken: "private-access", RefreshToken: "private-refresh", ExpiresIn: 3600,
		Extra: map[string]string{
			"kiro_auth_method": "external_idp", "kiro_login_flow": "hosted",
			"kiro_client_id": "client", "kiro_scope": "offline_access conversations",
			"kiro_token_endpoint": "https://login.microsoftonline.com/tenant/token",
			"kiro_profile_arn":    "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
			"kiro_region":         "us-east-1",
		},
	}
	id, err := s.persistOAuthAccount(httptest.NewRequest("POST", "/", nil), "kiro", "", tokens)
	require.NoError(t, err)
	require.Equal(t, 1, validator.calls, "hosted login must not inherit Kiro's legacy validation bypass")
	account, err := db.Accounts().Get(context.Background(), id)
	require.NoError(t, err)
	raw, err := json.Marshal(account)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private-access")
	require.NotContains(t, string(raw), "private-refresh")
	creds, err := s.vault.Open(account)
	require.NoError(t, err)
	require.Equal(t, tokens.AccessToken, creds.AccessToken)
	refreshToken, err := s.vault.OpenRefreshToken(account)
	require.NoError(t, err)
	require.Equal(t, tokens.RefreshToken, refreshToken)
	require.Equal(t, "hosted", creds.Extra["kiro_login_flow"])
	require.Equal(t, tokens.Extra["kiro_profile_arn"], creds.Extra["kiro_profile_arn"])
	require.Equal(t, tokens.Extra["kiro_token_endpoint"], creds.Extra["kiro_token_endpoint"])
	validator.err = errors.New("model access denied")
	_, err = s.persistOAuthAccount(httptest.NewRequest("POST", "/", nil), "kiro", "", tokens)
	require.ErrorContains(t, err, "model access denied")
	require.Equal(t, 2, validator.calls)
	s.conns = nil
	_, err = s.persistOAuthAccount(httptest.NewRequest("POST", "/", nil), "kiro", "", tokens)
	require.ErrorContains(t, err, "validation is unavailable")
}
