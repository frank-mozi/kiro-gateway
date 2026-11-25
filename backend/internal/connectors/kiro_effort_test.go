package connectors

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mydisha/keirouter/backend/internal/core"
	"github.com/mydisha/keirouter/backend/internal/transform"
	"github.com/stretchr/testify/require"
)

func TestKiroNativeEffortWire(t *testing.T) {
	const catalog = `{"models":[
	{"modelId":"claude","additionalModelRequestFieldsSchema":{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive","disabled"]}}},"output_config":{"properties":{"effort":{"enum":["low","xhigh","max"]}}}}}},
	{"modelId":"gpt","additionalModelRequestFieldsSchema":{"properties":{"reasoning":{"properties":{"effort":{"enum":["none","high","max"]}}}}}},
	{"modelId":"legacy"}]}`
	for _, tt := range []struct {
		name, model, input, fields string
		reject, responses          bool
	}{
		{"cc xhigh", "claude", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}`, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`, false, false},
		{"cc max", "claude", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, false, false},
		{"cc gpt", "gpt", `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`, `{"reasoning":{"effort":"high"}}`, false, false},
		{"codex none", "gpt", `,"reasoning":{"effort":"none"}`, `{"reasoning":{"effort":"none"}}`, false, true},
		{"omitted", "legacy", `,"thinking":{"type":"adaptive"}`, "", false, false},
		{"disabled", "claude", `,"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}`, "", false, false},
		{"title", "claude", `,"output_config":{"effort":"xhigh"}`, "", false, false},
		{"unlisted level", "gpt", `,"reasoning":{"effort":"xhigh"}`, "", true, true},
		{"missing schema", "legacy", `,"reasoning":{"effort":"high"}`, "", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var req *core.ChatRequest
			var err error
			if tt.responses {
				req, err = (transform.OpenAIResponsesCodec{}).ParseRequest([]byte(`{"model":"` + tt.model + `","input":"hi"` + tt.input + `}`))
			} else {
				req, err = (transform.AnthropicCodec{}).ParseRequest([]byte(`{"model":"` + tt.model + `","messages":[{"role":"user","content":"hi"}]` + tt.input + `}`))
			}
			require.NoError(t, err)
			old := sharedClient
			t.Cleanup(func() { sharedClient = old })
			var generated, listed int
			sharedClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					listed++
					require.Equal(t, "management.us-east-1.kiro.dev", r.URL.Host)
					require.Equal(t, "/List-Available-Models", r.URL.Path)
					require.Equal(t, "EXTERNAL_IDP", r.Header.Get("TokenType"))
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(catalog))}, nil
				}
				generated++
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				var body map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &body))
				if tt.fields == "" {
					require.NotContains(t, body, "additionalModelRequestFields")
				} else {
					require.JSONEq(t, tt.fields, string(body["additionalModelRequestFields"]))
				}
				require.NotContains(t, string(raw), "max_thinking_length")
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			c := NewKiro("kiro", "")
			stream, err := c.Stream(context.Background(), req, core.Credentials{AccessToken: "test", Extra: map[string]string{"kiro_auth_method": "external_idp"}}, core.StreamConfig{})
			if tt.reject {
				require.Error(t, err)
				require.Equal(t, core.ErrBadRequest, core.AsProviderError(err).Kind)
				require.Zero(t, generated)
				return
			}
			require.NoError(t, err)
			for range stream {
			}
			require.Equal(t, 1, generated)
			if tt.name == "omitted" {
				require.Zero(t, listed)
			}
		})
	}
}
