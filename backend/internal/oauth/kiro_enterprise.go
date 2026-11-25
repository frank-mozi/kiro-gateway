package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var kiroEnterprisePorts = []int{3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153}
var kiroRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`)
var kiroProfilePattern = regexp.MustCompile(`^arn:aws:codewhisperer:([a-z0-9-]+):[0-9]{12}:profile/[A-Za-z0-9_-]+$`)

type KiroEnterpriseStatus struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	URL       string    `json:"url,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Profiles  []string  `json:"profiles,omitempty"`
	AccountID string    `json:"account_id,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// KiroEnterpriseLogin owns one short-lived browser flow, separate from the
// device-code sessions: external identity providers do not support device flow.
type KiroEnterpriseLogin struct {
	mu      sync.Mutex
	current *kiroEnterpriseSession
}

type kiroEnterpriseSession struct {
	mu          sync.Mutex
	status      KiroEnterpriseStatus
	ctx         context.Context
	cancel      context.CancelFunc
	server      *http.Server
	log         *slog.Logger
	save        func(context.Context, *Tokens) (string, error)
	portal      PKCE
	idp         PKCE
	redirect    string
	region      string
	clientID    string
	scope       string
	endpoint    string
	issuer      string
	tokens      *Tokens
	tokenExpiry time.Time
}

func (m *KiroEnterpriseLogin) Start(region string, log *slog.Logger, save func(context.Context, *Tokens) (string, error)) (KiroEnterpriseStatus, error) {
	if region == "" {
		region = kiroDefaultRegion
	}
	if !kiroRegionPattern.MatchString(region) {
		return KiroEnterpriseStatus{}, fmt.Errorf("kiro: invalid AWS region")
	}
	portal, err := GeneratePKCE(32)
	if err != nil {
		return KiroEnterpriseStatus{}, err
	}
	id, err := GenerateState()
	if err != nil {
		return KiroEnterpriseStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil {
		if err := m.current.stop("cancelled"); err != nil {
			return KiroEnterpriseStatus{}, err
		}
	}
	listeners, port, err := listenKiroEnterprise(log)
	if err != nil {
		return KiroEnterpriseStatus{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	redirect := "http://localhost:" + strconv.Itoa(port)
	params := url.Values{
		"state": {portal.State}, "code_challenge": {portal.Challenge},
		"code_challenge_method": {"S256"}, "redirect_uri": {redirect}, "redirect_from": {"KiroIDE"},
	}
	session := &kiroEnterpriseSession{
		ctx: ctx, cancel: cancel, log: log, save: save, portal: portal,
		redirect: redirect, region: region,
		status: KiroEnterpriseStatus{
			ID: id, Phase: "waiting_for_kiro", URL: "https://app.kiro.dev/signin?" + params.Encode(),
			ExpiresAt: time.Now().Add(10 * time.Minute),
		},
	}
	session.server = &http.Server{
		Handler: http.HandlerFunc(session.callback), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 10 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	m.current = session
	for _, listener := range listeners {
		go func() {
			if err := session.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				session.fail("Kiro callback listener stopped unexpectedly")
			}
		}()
	}
	go func() {
		<-ctx.Done()
		session.mu.Lock()
		if !enterpriseTerminal(session.status.Phase) {
			session.status.Phase = "expired"
			session.status.Error = "Kiro sign-in expired; please start again"
			session.tokens = nil
			session.log.Warn("Kiro organization sign-in expired")
		}
		session.mu.Unlock()
		session.closeCallback()
	}()
	return session.snapshot(), nil
}

func listenKiroEnterprise(log *slog.Logger) ([]net.Listener, int, error) {
	for _, port := range kiroEnterprisePorts {
		v4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		port = v4.Addr().(*net.TCPAddr).Port
		v6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if err == nil {
			return []net.Listener{v4, v6}, port, nil
		}
		if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) {
			log.Warn("IPv6 loopback unavailable; Kiro callback uses IPv4 only")
			return []net.Listener{v4}, port, nil
		}
		// Do not send a localhost callback to a port owned by another IPv6 listener.
		_ = v4.Close()
	}
	return nil, 0, fmt.Errorf("kiro: no supported loopback callback port is available; close other Kiro sign-in windows and retry")
}

func (m *KiroEnterpriseLogin) session(id string) (*kiroEnterpriseSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || id == "" || m.current.status.ID != id {
		return nil, fmt.Errorf("kiro: sign-in session not found; please start again")
	}
	return m.current, nil
}

func (m *KiroEnterpriseLogin) Status(id string) (KiroEnterpriseStatus, error) {
	s, err := m.session(id)
	if err != nil {
		return KiroEnterpriseStatus{}, err
	}
	return s.snapshot(), nil
}

func (m *KiroEnterpriseLogin) Cancel(id string) error {
	s, err := m.session(id)
	if err != nil {
		return err
	}
	return s.stop("cancelled")
}

func (m *KiroEnterpriseLogin) SelectProfile(id, arn string) (KiroEnterpriseStatus, error) {
	s, err := m.session(id)
	if err != nil {
		return KiroEnterpriseStatus{}, err
	}
	if err := s.persist(arn); err != nil {
		return KiroEnterpriseStatus{}, err
	}
	return s.snapshot(), nil
}

func (s *kiroEnterpriseSession) snapshot() KiroEnterpriseStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.status
	result.Profiles = append([]string(nil), result.Profiles...)
	return result
}

func enterpriseTerminal(phase string) bool {
	return phase == "complete" || phase == "failed" || phase == "expired" || phase == "cancelled"
}

func (s *kiroEnterpriseSession) stop(phase string) error {
	s.mu.Lock()
	if s.status.Phase == "saving" {
		s.mu.Unlock()
		return fmt.Errorf("kiro: account is being saved; wait for completion")
	}
	if !enterpriseTerminal(s.status.Phase) {
		s.status.Phase = phase
		s.tokens = nil
	}
	s.mu.Unlock()
	s.cancel()
	s.closeCallback()
	return nil
}

func (s *kiroEnterpriseSession) closeCallback() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		s.log.Warn("Kiro callback shutdown timed out", "error", err)
		_ = s.server.Close()
	}
}

func (s *kiroEnterpriseSession) fail(message string) {
	s.mu.Lock()
	if !enterpriseTerminal(s.status.Phase) {
		s.status.Phase, s.status.Error = "failed", message
		s.tokens = nil
		s.log.Warn("Kiro organization sign-in failed", "error", message)
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *kiroEnterpriseSession) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	expectedHost := strings.TrimPrefix(s.redirect, "http://")
	if r.Host != expectedHost || len(r.URL.RawQuery) > 24<<10 {
		http.Error(w, "Invalid callback host or query", http.StatusBadRequest)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/signin/callback" && r.URL.Path != "/oauth/callback" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	s.mu.Lock()
	expectedState, expectedPhase := s.portal.State, "waiting_for_kiro"
	if r.URL.Path == "/oauth/callback" {
		expectedState, expectedPhase = s.idp.State, "waiting_for_company"
	}
	if expectedState == "" || len(q["state"]) != 1 || q.Get("state") != expectedState {
		s.mu.Unlock()
		http.Error(w, "Invalid sign-in state; return to Kiro Gateway", http.StatusBadRequest)
		return
	}
	if s.status.Phase != expectedPhase {
		s.mu.Unlock()
		http.Error(w, "Callback already handled or sign-in no longer active", http.StatusConflict)
		return
	}
	s.status.Phase = "processing"
	s.mu.Unlock()
	if q.Get("error") != "" {
		s.fail("Kiro or your organization declined sign-in; please retry in Kiro Gateway")
		http.Error(w, "Sign-in declined; return to Kiro Gateway", http.StatusBadRequest)
		return
	}
	if r.URL.Path != "/oauth/callback" {
		next, err := s.companyAuthorization(q)
		if err != nil {
			s.fail(err.Error())
			http.Error(w, "Cannot continue organization sign-in; see Kiro Gateway", http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	if err := s.exchange(q.Get("code")); err != nil {
		s.fail(err.Error())
		http.Error(w, "Sign-in could not be completed; see Kiro Gateway", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "Kiro authentication received. Return to Kiro Gateway to check account status or select your Kiro profile. You may close this tab.")
	go s.closeCallback()
}

func (s *kiroEnterpriseSession) companyAuthorization(q url.Values) (string, error) {
	if q.Get("login_option") != "external_idp" || q.Get("client_id") == "" || q.Get("scopes") == "" {
		return "", fmt.Errorf("kiro: choose Your Organization and your work email / Kiro Start URL; this entry is not a social or Builder ID login")
	}
	issuer, err := validateKiroEnterpriseEndpoint(q.Get("issuer_url"))
	if err != nil {
		return "", err
	}
	var discovery struct {
		Issuer        string `json:"issuer"`
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", fmt.Errorf("kiro: invalid organization discovery URL")
	}
	if err := kiroEnterpriseJSON(req, &discovery, "organization discovery"); err != nil {
		return "", err
	}
	if strings.TrimRight(discovery.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return "", fmt.Errorf("kiro: organization discovery issuer does not match Kiro's descriptor")
	}
	auth, err := validateKiroEnterpriseEndpoint(discovery.Authorization)
	if err != nil {
		return "", err
	}
	token, err := validateKiroEnterpriseEndpoint(discovery.Token)
	if err != nil {
		return "", err
	}
	pkce, err := GeneratePKCE(96)
	if err != nil {
		return "", err
	}
	params := url.Values{
		"client_id": {q.Get("client_id")}, "response_type": {"code"},
		"redirect_uri": {s.redirect + "/oauth/callback"}, "scope": {q.Get("scopes")},
		"code_challenge": {pkce.Challenge}, "code_challenge_method": {"S256"},
		"response_mode": {"query"}, "state": {pkce.State},
	}
	if hint := q.Get("login_hint"); hint != "" {
		params.Set("login_hint", hint)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return "", fmt.Errorf("kiro: sign-in cancelled or expired")
	}
	s.idp, s.issuer, s.endpoint = pkce, issuer, token
	s.clientID, s.scope = q.Get("client_id"), q.Get("scopes")
	s.status.Phase = "waiting_for_company"
	return auth + "?" + params.Encode(), nil
}

func (s *kiroEnterpriseSession) exchange(code string) error {
	if code == "" {
		return fmt.Errorf("kiro: organization callback contained no authorization code")
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {s.clientID},
		"code": {code}, "code_verifier": {s.idp.Verifier},
		"redirect_uri": {s.redirect + "/oauth/callback"}, "scope": {s.scope},
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("kiro: invalid organization token URL")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expiry  int    `json:"expires_in"`
	}
	if err := kiroEnterpriseJSON(req, &result, "organization token exchange"); err != nil {
		return err
	}
	if result.Access == "" || result.Refresh == "" || result.Expiry <= 0 {
		return fmt.Errorf("kiro: organization did not return expiring, refreshable credentials; check offline access consent and organization policy")
	}
	tokenExpiry := time.Now().Add(time.Duration(result.Expiry) * time.Second)
	profiles, err := kiroEnterpriseProfiles(s.ctx, result.Access, s.region)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return fmt.Errorf("kiro: sign-in cancelled or expired")
	}
	s.tokens = &Tokens{
		AccessToken: result.Access, RefreshToken: result.Refresh, ExpiresIn: result.Expiry,
		Email: externalJWTIdentity(result.Access),
		Extra: map[string]string{
			"kiro_auth_method": "external_idp", "kiro_login_flow": "hosted",
			"kiro_client_id": s.clientID, "kiro_scope": s.scope,
			"kiro_token_endpoint": s.endpoint, "kiro_issuer": s.issuer, "kiro_region": s.region,
		},
	}
	s.tokenExpiry = tokenExpiry
	s.status.Phase, s.status.Profiles = "select_profile", profiles
	s.mu.Unlock()
	if len(profiles) == 1 {
		return s.persist(profiles[0])
	}
	return nil
}

func (s *kiroEnterpriseSession) persist(arn string) error {
	s.mu.Lock()
	if s.status.Phase == "complete" {
		s.mu.Unlock()
		return nil
	}
	found := false
	for _, profile := range s.status.Profiles {
		found = found || profile == arn
	}
	if s.status.Phase != "select_profile" || !found || s.tokens == nil {
		s.mu.Unlock()
		return fmt.Errorf("kiro: select a profile returned by this sign-in session")
	}
	s.status.Phase = "saving"
	tokens := s.tokens
	tokens.ExpiresIn = max(1, int(time.Until(s.tokenExpiry).Seconds()))
	tokens.Extra["kiro_profile_arn"] = arn
	tokens.Extra["kiro_region"] = kiroProfilePattern.FindStringSubmatch(arn)[1]
	s.mu.Unlock()
	id, err := s.save(s.ctx, tokens)
	if err != nil {
		s.fail("Kiro credentials could not be validated or saved; check model access, subscription, connectivity and data-directory permissions")
		return fmt.Errorf("kiro: account validation or persistence failed")
	}
	s.mu.Lock()
	s.status.Phase, s.status.AccountID, s.tokens = "complete", id, nil
	s.mu.Unlock()
	s.log.Info("Kiro organization account connected", "account_id", id)
	s.cancel()
	return nil
}

func validateKiroEnterpriseEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return "", fmt.Errorf("kiro: organization endpoint must be HTTPS without credentials, query, fragment or a custom port")
	}
	host := strings.ToLower(u.Hostname())
	allowed := microsoftTokenEndpointHosts[host] || host == "login.microsoftonline.us" || host == "login.microsoftonline.cn"
	for _, suffix := range []string{".okta.com", ".okta-emea.com", ".oktapreview.com"} {
		allowed = allowed || strings.HasSuffix(host, suffix)
	}
	if !allowed {
		return "", fmt.Errorf("kiro: organization identity-provider host is not supported; supported hosts are Microsoft and Okta (custom domains require review)")
	}
	return u.String(), nil
}

func kiroEnterpriseJSON(req *http.Request, result any, operation string) error {
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// url.Error includes the URL; do not put callback data or token URLs in logs.
		return fmt.Errorf("kiro: %s network request failed; check the outbound proxy and connection", operation)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return fmt.Errorf("kiro: %s response unreadable or too large", operation)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kiro: %s returned HTTP %d: %s", operation, resp.StatusCode, kiroOAuthErrorCode(raw))
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("kiro: %s response is not valid JSON", operation)
	}
	return nil
}

func kiroEnterpriseProfiles(ctx context.Context, token, region string) ([]string, error) {
	profiles := []string{}
	seen := map[string]bool{}
	next := ""
	pages := map[string]bool{}
	for page := 0; page < 20; page++ {
		payload := struct {
			NextToken string `json:"nextToken,omitempty"`
		}{NextToken: next}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("kiro: cannot encode profile discovery request")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://codewhisperer."+region+".amazonaws.com/", strings.NewReader(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("kiro: invalid profile service URL")
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("TokenType", "EXTERNAL_IDP")
		req.Header.Set("User-Agent", "AWS-SDK-JS/3.0.0 kiro-ide/1.0.0")
		req.Header.Set("X-Amz-User-Agent", "aws-sdk-js/3.0.0 kiro-ide/1.0.0")
		var result struct {
			Profiles []struct {
				ARN string `json:"arn"`
			} `json:"profiles"`
			NextToken string `json:"nextToken"`
		}
		if err := kiroEnterpriseJSON(req, &result, "Kiro profile discovery"); err != nil {
			return nil, err
		}
		for _, profile := range result.Profiles {
			if !kiroProfilePattern.MatchString(profile.ARN) {
				return nil, fmt.Errorf("kiro: profile service returned an invalid profile ARN")
			}
			if !seen[profile.ARN] {
				profiles = append(profiles, profile.ARN)
				seen[profile.ARN] = true
			}
		}
		next = result.NextToken
		if next == "" {
			if len(profiles) == 0 {
				return nil, fmt.Errorf("kiro: no profile is available in this region; check your Kiro subscription, organization assignment and region")
			}
			return profiles, nil
		}
		if pages[next] {
			return nil, fmt.Errorf("kiro: profile service repeated a pagination token")
		}
		pages[next] = true
	}
	return nil, fmt.Errorf("kiro: profile discovery exceeded the 20-page safety limit; no partial list was selected")
}
