package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const enterpriseTestIssuer = "https://login.microsoftonline.com/tenant/v2.0"
const enterpriseTestProfile = "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"

func enterpriseFixture(t *testing.T, profiles string, save func(context.Context, *Tokens) (string, error)) (*KiroEnterpriseLogin, *kiroEnterpriseSession) {
	t.Helper()
	oldClient, oldPorts := httpClient, kiroEnterprisePorts
	kiroEnterprisePorts = []int{0}
	httpClient = &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.String() {
		case enterpriseTestIssuer + "/.well-known/openid-configuration":
			body = fmt.Sprintf(`{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q}`,
				enterpriseTestIssuer, enterpriseTestIssuer+"/authorize", enterpriseTestIssuer+"/token")
		case enterpriseTestIssuer + "/token":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "authorization_code", r.Form.Get("grant_type"))
			require.Equal(t, "company-code", r.Form.Get("code"))
			require.Equal(t, "kiro-company-client", r.Form.Get("client_id"))
			require.Len(t, r.Form.Get("code_verifier"), 128)
			require.True(t, strings.HasSuffix(r.Form.Get("redirect_uri"), "/oauth/callback"))
			require.Equal(t, "offline_access conversations", r.Form.Get("scope"))
			body = `{"access_token":"private-access","refresh_token":"private-refresh","expires_in":3600}`
		case "https://codewhisperer.us-east-1.amazonaws.com/":
			require.Equal(t, "EXTERNAL_IDP", r.Header.Get("TokenType"))
			require.Equal(t, "Bearer private-access", r.Header.Get("Authorization"))
			require.Equal(t, "AmazonCodeWhispererService.ListAvailableProfiles", r.Header.Get("X-Amz-Target"))
			body = `{"profiles":` + profiles + `}`
		default:
			t.Errorf("unexpected outbound request host/path: %s %s", r.URL.Host, r.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	t.Cleanup(func() { httpClient, kiroEnterprisePorts = oldClient, oldPorts })
	manager := &KiroEnterpriseLogin{}
	status, err := manager.Start("", slog.New(slog.NewTextHandler(io.Discard, nil)), save)
	require.NoError(t, err)
	session, err := manager.session(status.ID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Cancel(status.ID)) })
	return manager, session
}

func enterpriseCallback(s *kiroEnterpriseSession, path string, q url.Values) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	s.callback(response, httptest.NewRequest(http.MethodGet, s.redirect+path+"?"+q.Encode(), nil))
	return response
}

func enterpriseDescriptor(s *kiroEnterpriseSession) url.Values {
	return url.Values{
		"state": {s.portal.State}, "login_option": {"external_idp"},
		"issuer_url": {enterpriseTestIssuer}, "client_id": {"kiro-company-client"},
		"scopes": {"offline_access conversations"},
	}
}

func enterpriseAuthorize(t *testing.T, s *kiroEnterpriseSession) url.Values {
	t.Helper()
	response := enterpriseCallback(s, "/", enterpriseDescriptor(s))
	require.Equal(t, http.StatusFound, response.Code, response.Body.String())
	location, err := url.Parse(response.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, enterpriseTestIssuer+"/authorize", location.Scheme+"://"+location.Host+location.Path)
	return location.Query()
}

func TestKiroEnterpriseTwoLegLogin(t *testing.T) {
	var saved *Tokens
	var saves int
	manager, session := enterpriseFixture(t, `[{"arn":"`+enterpriseTestProfile+`"}]`, func(_ context.Context, tokens *Tokens) (string, error) {
		saved = tokens
		saves++
		return "saved-account", nil
	})
	start := session.snapshot()
	portal, err := url.Parse(start.URL)
	require.NoError(t, err)
	require.Equal(t, "https://app.kiro.dev/signin", portal.Scheme+"://"+portal.Host+portal.Path)
	require.Len(t, portal.Query(), 5)
	require.Equal(t, "KiroIDE", portal.Query().Get("redirect_from"))
	require.Equal(t, session.redirect, portal.Query().Get("redirect_uri"))
	require.NotEqual(t, start.ID, portal.Query().Get("state"))

	wrong := enterpriseDescriptor(session)
	wrong.Set("state", "wrong")
	require.Equal(t, 400, enterpriseCallback(session, "/", wrong).Code)
	require.Equal(t, "waiting_for_kiro", session.snapshot().Phase)

	company := enterpriseAuthorize(t, session)
	require.NotEqual(t, session.portal.State, company.Get("state"))
	require.Equal(t, session.redirect+"/oauth/callback", company.Get("redirect_uri"))
	digest := sha256.Sum256([]byte(session.idp.Verifier))
	require.Equal(t, base64.RawURLEncoding.EncodeToString(digest[:]), company.Get("code_challenge"))
	require.Equal(t, "S256", company.Get("code_challenge_method"))
	require.Equal(t, 409, enterpriseCallback(session, "/", enterpriseDescriptor(session)).Code)
	require.Equal(t, 400, enterpriseCallback(session, "/oauth/callback", url.Values{"state": {"wrong"}, "error": {"access_denied"}}).Code)
	require.Equal(t, "waiting_for_company", session.snapshot().Phase)

	callback := url.Values{"state": {company.Get("state")}, "code": {"company-code"}}
	require.Equal(t, 200, enterpriseCallback(session, "/oauth/callback", callback).Code)
	require.Equal(t, 409, enterpriseCallback(session, "/oauth/callback", callback).Code)
	require.Equal(t, 1, saves)
	require.Equal(t, "private-refresh", saved.RefreshToken)
	require.Equal(t, "external_idp", saved.Extra["kiro_auth_method"])
	require.Equal(t, "hosted", saved.Extra["kiro_login_flow"])
	require.Equal(t, enterpriseTestProfile, saved.Extra["kiro_profile_arn"])
	require.Equal(t, enterpriseTestIssuer+"/token", saved.Extra["kiro_token_endpoint"])
	result, err := manager.Status(start.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", result.Phase)
	require.Equal(t, "saved-account", result.AccountID)
	require.Nil(t, session.tokens)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	for _, secret := range []string{"private-access", "private-refresh", session.idp.Verifier, "company-code"} {
		require.NotContains(t, string(raw), secret)
	}
}

func TestKiroEnterpriseMultipleProfilesAndSaveFailure(t *testing.T) {
	second := strings.Replace(enterpriseTestProfile, "profile/test", "profile/second", 1)
	var saves int
	manager, session := enterpriseFixture(t, `[{"arn":"`+enterpriseTestProfile+`"},{"arn":"`+second+`"}]`, func(_ context.Context, tokens *Tokens) (string, error) {
		saves++
		require.Equal(t, second, tokens.Extra["kiro_profile_arn"])
		return "", fmt.Errorf("untrusted response containing private-access")
	})
	company := enterpriseAuthorize(t, session)
	require.Equal(t, 200, enterpriseCallback(session, "/oauth/callback", url.Values{
		"state": {company.Get("state")}, "code": {"company-code"},
	}).Code)
	require.Equal(t, "select_profile", session.snapshot().Phase)
	require.Zero(t, saves)
	_, err := manager.SelectProfile(session.status.ID, "not-an-authorized-profile")
	require.Error(t, err)
	_, err = manager.SelectProfile(session.status.ID, second)
	require.Error(t, err)
	status := session.snapshot()
	require.Equal(t, "failed", status.Phase)
	require.NotContains(t, status.Error, "private-access")
	require.Nil(t, session.tokens)
	require.Equal(t, 1, saves)
}

func TestKiroEnterpriseCancelExpiryAndErrors(t *testing.T) {
	for _, mode := range []string{"cancel", "expiry", "provider-error", "no-profiles", "social"} {
		t.Run(mode, func(t *testing.T) {
			manager, session := enterpriseFixture(t, `[]`, func(context.Context, *Tokens) (string, error) {
				t.Fatal("failed flow must not persist credentials")
				return "", nil
			})
			id := session.snapshot().ID
			_, err := manager.Status("unknown")
			require.Error(t, err)
			switch mode {
			case "cancel":
				require.NoError(t, manager.Cancel(id))
				require.Equal(t, "cancelled", session.snapshot().Phase)
			case "expiry":
				session.cancel()
				require.Eventually(t, func() bool { return session.snapshot().Phase == "expired" }, time.Second, time.Millisecond)
			case "provider-error":
				descriptor := enterpriseDescriptor(session)
				descriptor.Set("error", "access_denied")
				descriptor.Set("error_description", "private-token")
				require.Equal(t, 400, enterpriseCallback(session, "/", descriptor).Code)
				require.Equal(t, "failed", session.snapshot().Phase)
				require.NotContains(t, session.snapshot().Error, "private-token")
			case "social":
				require.Equal(t, 502, enterpriseCallback(session, "/", url.Values{
					"state": {session.portal.State}, "code": {"not-an-enterprise-descriptor"},
				}).Code)
				require.Contains(t, session.snapshot().Error, "Your Organization")
			case "no-profiles":
				company := enterpriseAuthorize(t, session)
				require.Equal(t, 502, enterpriseCallback(session, "/oauth/callback", url.Values{
					"state": {company.Get("state")}, "code": {"company-code"},
				}).Code)
				require.Equal(t, "failed", session.snapshot().Phase)
				require.Contains(t, session.snapshot().Error, "no profile")
			}
			require.Nil(t, session.tokens)
		})
	}
}

func TestKiroEnterpriseEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{
		"http://login.microsoftonline.com/token", "https://127.0.0.1/token",
		"https://login.microsoftonline.com.evil.example/token", "https://evilokta.com/token",
		"https://name@tenant.okta.com/token", "https://tenant.okta.com:8443/token",
		"https://tenant.okta.com/token?secret=value", "https://tenant.okta.com/token#fragment",
	} {
		_, err := validateKiroEnterpriseEndpoint(endpoint)
		require.Error(t, err, endpoint)
	}
	for _, endpoint := range []string{enterpriseTestIssuer, "https://tenant.okta.com/oauth2/default", "https://login.microsoftonline.us/tenant/v2.0"} {
		_, err := validateKiroEnterpriseEndpoint(endpoint)
		require.NoError(t, err, endpoint)
	}
}

func TestKiroEnterpriseRejectsRedirectAndMissingRefresh(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"redirect", 302, "do not follow this"},
		{"missing refresh", 200, `{"access_token":"private-access","expires_in":3600}`},
		{"error body", 401, `{"error":"private-token"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, session := enterpriseFixture(t, `[]`, func(context.Context, *Tokens) (string, error) {
				t.Fatal("invalid tokens must not persist")
				return "", nil
			})
			company := enterpriseAuthorize(t, session)
			var calls atomic.Int32
			httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{
					StatusCode: test.code, Header: http.Header{"Location": {"https://example.org/stolen"}},
					Body: io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})
			require.Equal(t, 502, enterpriseCallback(session, "/oauth/callback", url.Values{
				"state": {company.Get("state")}, "code": {"company-code"},
			}).Code)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, "failed", session.snapshot().Phase)
			require.NotContains(t, session.snapshot().Error, "private-")
		})
	}
}

func TestKiroEnterpriseHostedRefresh(t *testing.T) {
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "tenant.okta.com", r.URL.Host)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "stored-refresh", r.Form.Get("refresh_token"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fresh","expires_in":3600}`))}, nil
	})}
	tokens, err := KiroExternalIDPRefresh(context.Background(), "stored-refresh", map[string]string{
		"kiro_login_flow": "hosted", "kiro_client_id": "kiro-client",
		"kiro_token_endpoint": "https://tenant.okta.com/token", "kiro_scope": "offline_access",
	})
	require.NoError(t, err)
	require.Equal(t, "fresh", tokens.AccessToken)
	require.Equal(t, "stored-refresh", tokens.RefreshToken)
}

func TestKiroEnterpriseRefreshPreservesReconnectSignal(t *testing.T) {
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(
			`{"error":"invalid_grant","error_description":"private-refresh must never appear in logs"}`,
		))}, nil
	})}
	_, err := KiroExternalIDPRefresh(context.Background(), "private-refresh", map[string]string{
		"kiro_client_id": "client", "kiro_scope": "offline_access",
		"kiro_token_endpoint": enterpriseTestIssuer + "/token",
	})
	require.Error(t, err)
	require.True(t, IsPermanentRefresh(err))
	require.NotContains(t, err.Error(), "private-refresh")
}

func TestKiroEnterpriseProfilePagination(t *testing.T) {
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	second := strings.Replace(enterpriseTestProfile, "profile/test", "profile/second", 1)
	var calls int
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"profiles":[{"arn":"` + enterpriseTestProfile + `"}],"nextToken":"page-two"}`
		if calls == 2 {
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.JSONEq(t, `{"nextToken":"page-two"}`, string(raw))
			body = `{"profiles":[{"arn":"` + enterpriseTestProfile + `"},{"arn":"` + second + `"}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	profiles, err := kiroEnterpriseProfiles(context.Background(), "token", "us-east-1")
	require.NoError(t, err)
	require.Equal(t, []string{enterpriseTestProfile, second}, profiles)
	require.Equal(t, 2, calls)
}
