package transform

import (
	"strings"

	"github.com/mydisha/keirouter/backend/internal/core"
	json "github.com/mydisha/keirouter/backend/internal/fastjson"
)

func hostedSearchType(typ string) bool {
	return typ == "web_search" || strings.HasPrefix(typ, "web_search_")
}

type toolRef struct{ Type, Name string }

func recordSearchTool(req *core.ChatRequest, tool, choice json.RawMessage, sources bool) {
	if req.HostedWebSearch == nil {
		req.HostedWebSearch = &core.HostedWebSearchRequest{ToolChoice: choice, IncludeSources: sources}
	}
	req.HostedWebSearch.Tools = append(req.HostedWebSearch.Tools, tool)
}

func hostedSearchTool(raw json.RawMessage) (toolRef, bool) {
	var t toolRef
	if len(raw) == 0 || json.Unmarshal(raw, &t) != nil {
		return t, false
	}
	return t, hostedSearchType(t.Type)
}

// An auto choice advertises availability; it does not require execution.
// A named client function called WebSearch is not a provider-hosted tool.
func requiresHostedSearch(choice json.RawMessage, tools []json.RawMessage) bool {
	if len(choice) == 0 {
		return false
	}
	var c struct {
		Type, Name, Mode string
		Tools            []json.RawMessage
	}
	if choice[0] == '"' {
		if json.Unmarshal(choice, &c.Type) != nil {
			return false
		}
	} else if json.Unmarshal(choice, &c) != nil {
		return false
	}
	switch c.Type {
	case "tool": // Anthropic selects a tool by its declared name.
		for _, raw := range tools {
			if t, hosted := hostedSearchTool(raw); hosted && c.Name != "" && t.Name == c.Name {
				return true
			}
		}
		return false
	case "allowed_tools":
		if c.Mode != "required" {
			return false
		}
		tools = c.Tools
	case "required", "any":
	default:
		return hostedSearchType(c.Type)
	}
	for _, raw := range tools {
		if _, hosted := hostedSearchTool(raw); !hosted {
			return false
		}
	}
	return len(tools) > 0
}

// Tool-choice translation. The canonical request stores tool_choice in the
// OpenAI form (the string "auto"/"none"/"required" or an object
// {type:"function",function:{name}}). Inbound non-OpenAI dialects normalize
// their own shape into this form at parse time, and each upstream codec renders
// it back into the shape that provider expects. Keeping a single canonical form
// means a forced/!auto tool choice survives any cross-dialect hop instead of
// being silently dropped.

// claudeToolChoiceToOpenAI converts an Anthropic tool_choice value into the
// canonical OpenAI form. Returns nil when absent or unrecognized.
func claudeToolChoiceToOpenAI(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		if name, _ := m["name"].(string); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

// openAIToolChoiceToClaude converts the canonical OpenAI tool_choice into the
// Anthropic form. Returns nil to omit the field (Anthropic then defaults to
// "auto"), so an unrecognized value never leaks upstream as a 400.
func openAIToolChoiceToClaude(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		case "auto":
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		// OpenAI forced tool: {type:"function", function:{name}}. Checked before
		// the native pass-through because this shape also carries a `type`
		// ("function") that Anthropic rejects.
		if fn, ok := t["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		// Already Anthropic-native — only pass through the types it accepts.
		switch typ, _ := t["type"].(string); typ {
		case "auto", "any", "tool", "none":
			return t
		}
	}
	return nil
}

// openAIToolChoiceToGemini converts the canonical OpenAI tool_choice into a
// Gemini toolConfig.functionCallingConfig. sanitize brings a forced tool name
// into Gemini's allowed character set. Returns nil to omit the field (Gemini
// then defaults to AUTO).
func openAIToolChoiceToGemini(v any, sanitize func(string) string) map[string]any {
	mode := func(m string) map[string]any {
		return map[string]any{"functionCallingConfig": map[string]any{"mode": m}}
	}
	switch t := v.(type) {
	case string:
		switch t {
		case "none":
			return mode("NONE")
		case "required":
			return mode("ANY")
		case "auto":
			return mode("AUTO")
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{
					"functionCallingConfig": map[string]any{
						"mode":                 "ANY",
						"allowedFunctionNames": []string{sanitize(name)},
					},
				}
			}
		}
	}
	return nil
}
