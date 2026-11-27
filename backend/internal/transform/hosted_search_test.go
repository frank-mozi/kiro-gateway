package transform

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mydisha/keirouter/backend/internal/core"
	"github.com/stretchr/testify/require"
)

func exampleSearchReply() *core.ChatResponse {
	return &core.ChatResponse{
		ID: "turn1", Model: "example-model", FinishReason: core.FinishStop,
		Message: core.Message{Role: core.RoleAssistant, Content: []core.ContentPart{{Type: core.PartText, Text: "See 文档."}}},
		Usage:   core.Usage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12, WebSearchRequests: 1},
		HostedSearch: &core.HostedSearchReceipt{
			ID: "search1", Query: "example release", CreatedAt: 1_800_000_000,
			Request:   &core.HostedWebSearchRequest{Tools: []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}},
			Sources:   []core.WebSearchSource{{URL: "https://example.invalid/release", Title: "Release notes", EncryptedContent: "test-only-opaque-result"}},
			Citations: []core.WebSearchCitation{{SourceIndex: 0, Start: 4, End: 6, CitedText: "Release notes", EncryptedIndex: "test-only-opaque-index"}},
		},
	}
}

type searchEvent struct {
	Type     string
	Seq      int             `json:"sequence_number"`
	Response json.RawMessage `json:"response"`
	Block    struct {
		Type, ID  string
		ToolUseID string           `json:"tool_use_id"`
		Content   []map[string]any `json:"content"`
	} `json:"content_block"`
	Delta      json.RawMessage
	Annotation map[string]any
	ItemID     string `json:"item_id"`
	Index      int
}

// Exercise the client codecs over loopback TLS. The upstream search receipt is
// a fixture: this test neither grants permission nor contacts a search service.
func TestHostedSearchClientProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, path, payload, tool string
		codec                     Codec
		sources                   bool
	}{
		{"Claude", "/v1/messages", `"messages":[{"role":"user","content":"example"}],"tool_choice":{"type":"auto"}`, `{"type":"web_search_20250305","name":"web_search","max_uses":1,"allowed_domains":["example.invalid"]}`, AnthropicCodec{}, false},
		{"Codex", "/v1/responses", `"input":"example","tool_choice":"auto","include":["web_search_call.action.sources"]`, `{"type":"web_search","external_web_access":true,"filters":{"allowed_domains":["example.invalid"]}}`, OpenAIResponsesCodec{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Method != http.MethodPost {
					http.NotFound(w, r)
					return
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "body read failed", 400)
					return
				}
				req, err := tc.codec.ParseRequest(raw)
				if err != nil || req.HostedWebSearch == nil || len(req.HostedWebSearch.Tools) != 1 || req.RequiresHostedWebSearch {
					http.Error(w, "native tool lost or converted to a required search", 400)
					return
				}
				if string(req.HostedWebSearch.Tools[0]) != tc.tool || req.HostedWebSearch.IncludeSources != tc.sources || len(req.HostedWebSearch.ToolChoice) == 0 {
					http.Error(w, "search options lost", 400)
					return
				}
				reply := exampleSearchReply()
				reply.HostedSearch.Request = req.HostedWebSearch
				if req.Stream {
					frames, err := RenderHostedSearchStream(tc.codec.Dialect(), reply)
					if err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(bytes.Join(frames, nil))
					return
				}
				body, err := tc.codec.RenderResponse(reply)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer s.Close()
			post := func(stream string) []byte {
				t.Helper()
				b := `{"model":"example-model",` + tc.payload + `,"tools":[` + tc.tool + `],"stream":` + stream + `}`
				res, err := s.Client().Post(s.URL+tc.path, "application/json", strings.NewReader(b))
				require.NoError(t, err)
				defer res.Body.Close()
				body, err := io.ReadAll(res.Body)
				require.NoError(t, err)
				require.Equal(t, 200, res.StatusCode, string(body))
				return body
			}
			body := post("false")
			var events []searchEvent
			for _, line := range strings.Split(string(post("true")), "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					var e searchEvent
					require.NoError(t, json.Unmarshal([]byte(data), &e))
					events = append(events, e)
				}
			}
			require.NotEmpty(t, events)
			const source = "https://example.invalid/release"
			if tc.sources {
				var reply struct {
					Output []struct {
						Type, Status string
						Action       struct{ Sources []map[string]string }
						Content      []struct{ Annotations []map[string]any }
					}
				}
				require.NoError(t, json.Unmarshal(body, &reply))
				require.Len(t, reply.Output, 2)
				require.Equal(t, "web_search_call", reply.Output[0].Type)
				require.Equal(t, "completed", reply.Output[0].Status)
				require.Equal(t, []map[string]string{{"type": "url", "url": source}}, reply.Output[0].Action.Sources)
				require.Len(t, reply.Output[1].Content, 1)
				require.Len(t, reply.Output[1].Content[0].Annotations, 1)
				cite := reply.Output[1].Content[0].Annotations[0]
				require.Equal(t, "url_citation", cite["type"])
				require.Equal(t, source, cite["url"])
				require.Equal(t, float64(4), cite["start_index"])
				require.Equal(t, float64(6), cite["end_index"])
				var done, annotated int
				for i, e := range events {
					require.Equal(t, i+1, e.Seq)
					if e.Type == "response.web_search_call.completed" {
						done++
						require.Equal(t, "ws_search1", e.ItemID)
					}
					if e.Type == "response.output_text.annotation.added" {
						annotated++
						require.Equal(t, cite, e.Annotation)
					}
				}
				require.Equal(t, 1, done)
				require.Equal(t, 1, annotated)
				last := events[len(events)-1]
				require.Equal(t, "response.completed", last.Type)
				require.JSONEq(t, string(body), string(last.Response))
			} else {
				var reply struct {
					Content []struct {
						Type, ID, Text string
						ToolUseID      string `json:"tool_use_id"`
						Content        []map[string]any
						Citations      []map[string]any
					}
				}
				require.NoError(t, json.Unmarshal(body, &reply))
				require.Len(t, reply.Content, 5)
				require.Equal(t, "server_tool_use", reply.Content[0].Type)
				require.Equal(t, "web_search_tool_result", reply.Content[1].Type)
				require.Equal(t, reply.Content[0].ID, reply.Content[1].ToolUseID)
				require.Len(t, reply.Content[1].Content, 1)
				require.Equal(t, source, reply.Content[1].Content[0]["url"])
				require.Equal(t, "test-only-opaque-result", reply.Content[1].Content[0]["encrypted_content"])
				require.Equal(t, "文档", reply.Content[3].Text)
				require.Len(t, reply.Content[3].Citations, 1)
				cite := reply.Content[3].Citations[0]
				require.Equal(t, source, cite["url"])
				require.Equal(t, "test-only-opaque-index", cite["encrypted_index"])
				var starts, stops, cited int
				for _, e := range events {
					switch e.Type {
					case "content_block_start":
						require.Equal(t, starts, e.Index)
						require.Equal(t, reply.Content[starts].Type, e.Block.Type)
						starts++
					case "content_block_stop":
						require.Equal(t, stops, e.Index)
						stops++
					case "content_block_delta":
						var delta struct {
							Type     string
							Citation map[string]any
						}
						require.NoError(t, json.Unmarshal(e.Delta, &delta))
						if delta.Type == "citations_delta" {
							cited++
							require.Equal(t, cite, delta.Citation)
						}
					}
				}
				require.Equal(t, len(reply.Content), starts)
				require.Equal(t, starts, stops)
				require.Equal(t, 1, cited)
				require.Equal(t, "message_start", events[0].Type)
				require.Equal(t, "message_stop", events[len(events)-1].Type)
			}
		})
	}
}

func TestHostedSearchRejectsUnbackedCitations(t *testing.T) {
	r := exampleSearchReply()
	r.HostedSearch.Citations[0].SourceIndex = 1
	for _, d := range []core.Dialect{core.DialectAnthropic, core.DialectOpenAIResponses} {
		frames, err := RenderHostedSearchStream(d, r)
		require.Error(t, err)
		require.Empty(t, frames)
	}
	r = exampleSearchReply()
	r.HostedSearch.Sources[0].EncryptedContent = ""
	b, err := (AnthropicCodec{}).RenderResponse(r)
	require.Error(t, err)
	require.Empty(t, b)
	b, err = (OpenAIResponsesCodec{}).RenderResponse(r)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"sources"`) // include was not requested
}
