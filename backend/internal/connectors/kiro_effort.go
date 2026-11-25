package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mydisha/keirouter/backend/internal/core"
	"github.com/mydisha/keirouter/backend/internal/transform"
)

var kiroEffortRegion = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]+$`)

type kiroFieldSchema struct {
	Properties map[string]struct {
		Properties map[string]struct {
			Enum []json.RawMessage `json:"enum"`
		} `json:"properties"`
	} `json:"properties"`
}

func (s kiroFieldSchema) strings(path, field string) []string {
	var values []string
	for _, raw := range s.Properties[path].Properties[field].Enum {
		var value string
		if json.Unmarshal(raw, &value) == nil && value != "" && value != "default" {
			values = append(values, value)
		}
	}
	return values
}

func (c *Kiro) nativeEffort(ctx context.Context, req *core.ChatRequest, creds core.Credentials) (map[string]any, error) {
	level := req.ClientEffort
	if level == "" || req.ClientThinkingType == "disabled" {
		return nil, nil
	}
	models, err := c.listModels(ctx, creds, isKiroExternalIDP(creds))
	if err != nil {
		return nil, fmt.Errorf("cannot verify Kiro effort capabilities: %w", err)
	}
	id := transform.KiroBaseModel(req.Model)
	reject := func(msg string) error {
		return &core.ProviderError{Kind: core.ErrBadRequest, Scope: core.FailureScopeRequest, StatusCode: 400, Provider: c.id, Model: id, Message: msg}
	}
	for _, model := range models {
		if model.ID != id || model.kiroEffortSchema == "" {
			continue
		}
		var schema kiroFieldSchema
		if err := json.Unmarshal([]byte(model.kiroEffortSchema), &schema); err != nil {
			return nil, reject("cannot read native effort schema for " + id)
		}
		adaptive := slices.Contains(schema.strings("thinking", "type"), "adaptive")
		for _, path := range []string{"output_config", "reasoning"} {
			levels := schema.strings(path, "effort")
			if len(levels) == 0 || (path == "output_config" && !adaptive) {
				continue
			}
			if adaptive && req.AnthropicThinkingInput && req.ClientThinkingType != "enabled" && req.ClientThinkingType != "adaptive" {
				return nil, nil // e.g. CC title requests
			}
			if !slices.Contains(levels, level) || level == "default" {
				return nil, reject(fmt.Sprintf("%s does not support requested effort %q (catalog: %s)", id, level, strings.Join(levels, ", ")))
			}
			fields := map[string]any{path: map[string]string{"effort": level}}
			if adaptive {
				fields["thinking"] = map[string]string{"type": "adaptive"}
			}
			return fields, nil
		}
	}
	return nil, reject("Kiro catalog does not declare native effort support for " + id + "; omit effort to use the Kiro default")
}
