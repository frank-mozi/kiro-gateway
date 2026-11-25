package portable

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type State struct {
	Managed         bool   `json:"managed"`
	Current         string `json:"current"`
	Launcher        string `json:"launcher"`
	Root            string `json:"root"`
	Source          Source `json:"source"`
	Latest          string `json:"latest"`
	Notes           string `json:"notes"`
	CheckedAt       string `json:"checked_at"`
	UpdateAvailable bool   `json:"update_available"`
	Phase           string `json:"phase"`
	Error           string `json:"error"`
}

type Client struct {
	address  string
	token    string
	instance string
	http     *http.Client
	mu       sync.Mutex
	draining bool
	active   int
}

func ClientFromEnvironment() (*Client, error) {
	address, token, instance := os.Getenv("KEIROUTER_PORTABLE_CONTROL"), os.Getenv("KEIROUTER_PORTABLE_TOKEN"), os.Getenv("KEIROUTER_PORTABLE_INSTANCE")
	if address == "" && token == "" && instance == "" {
		return nil, nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || len(token) != 64 || len(instance) != 64 {
		return nil, errors.New("invalid portable launcher environment; start with the portable command")
	}
	return &Client{
		address: address, token: token, instance: instance,
		http:     &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}},
		draining: os.Getenv("KEIROUTER_PORTABLE_HOLD") == "true",
	}, nil
}

func (c *Client) Call(ctx context.Context, method, path string, body any) (State, error) {
	var state State
	raw, err := json.Marshal(body)
	if err != nil {
		return state, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.address+path, bytes.NewReader(raw))
	if err != nil {
		return state, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return state, fmt.Errorf("portable launcher unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&failure); err != nil {
			return state, fmt.Errorf("launcher returned HTTP %d", resp.StatusCode)
		}
		return state, errors.New(failure.Error)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifest)).Decode(&state); err != nil {
		return state, err
	}
	return state, nil
}

func authorized(r *http.Request, token string) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

// Guard counts in-flight model requests and rejects new work while updating.
func (c *Client) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		c.mu.Lock()
		if c.draining {
			c.mu.Unlock()
			writeFailure(w, http.StatusServiceUnavailable, "portable update is draining requests; retry after restart")
			return
		}
		c.active++
		c.mu.Unlock()
		defer func() { c.mu.Lock(); c.active--; c.mu.Unlock() }()
		next.ServeHTTP(w, r)
	})
}

func (c *Client) ControlHandler(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, c.token) {
			writeFailure(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_portable/health":
			writeResponse(w, map[string]string{"version": version, "instance": c.instance})
		case r.Method == http.MethodPost && r.URL.Path == "/_portable/resume":
			c.mu.Lock()
			c.draining = false
			c.mu.Unlock()
			writeResponse(w, map[string]bool{"ok": true})
		case r.Method == http.MethodPost && r.URL.Path == "/_portable/drain":
			c.mu.Lock()
			c.draining = true
			c.mu.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				c.mu.Lock()
				active := c.active
				c.mu.Unlock()
				if active == 0 {
					writeResponse(w, map[string]bool{"ok": true})
					return
				}
				select {
				case <-ctx.Done():
					c.mu.Lock()
					c.draining = false
					c.mu.Unlock()
					writeFailure(w, http.StatusConflict, "active model requests did not finish in 60 seconds; upgrade cancelled")
					return
				case <-ticker.C:
				}
			}
		default:
			writeFailure(w, http.StatusNotFound, "unknown portable operation")
		}
	})
}

func writeResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func writeFailure(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func decodeBody(w http.ResponseWriter, r *http.Request, value any) bool {
	reader := http.MaxBytesReader(w, r.Body, 16384)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeFailure(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeFailure(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func privateEnvironment(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[strings.ToUpper(name)]; !replace {
			env = append(env, entry)
		}
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return env
}
