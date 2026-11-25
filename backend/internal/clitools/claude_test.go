package clitools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestClaudeModelPickerMerge(t *testing.T) {
	tests := []struct {
		name   string
		seed   string
		models []string
		want   string
	}{
		{"new", `{}`, []string{"kiro/sonnet"},
			`{"options":[{"model":"kiro/sonnet","label":"kiro/sonnet"}]}`},
		{"missing options", `{"modelPicker":{"replaceBuiltInOptions":true}}`, []string{"kiro/sonnet"},
			`{"replaceBuiltInOptions":true,"options":[{"model":"kiro/sonnet","label":"kiro/sonnet"}]}`},
		{"preserve and append", `{"permissions":{"allow":["Read"]},"env":{"CUSTOM":"keep"},"modelPicker":{"replaceBuiltInOptions":true,"futureField":"keep","options":[{"model":"kiro/opus","label":"Production","description":"Keep this","behavesAs":"claude-opus-4-6"}]}}`,
			[]string{"kiro/sonnet", "kiro/haiku", "kiro/opus"},
			`{"replaceBuiltInOptions":true,"futureField":"keep","options":[{"model":"kiro/opus","label":"Production","description":"Keep this","behavesAs":"claude-opus-4-6"},{"model":"kiro/sonnet","label":"kiro/sonnet"},{"model":"kiro/haiku","label":"kiro/haiku"}]}`},
		{"existing without label", `{"modelPicker":{"options":[{"model":"kiro/sonnet"}]}}`, []string{"kiro/sonnet"},
			`{"options":[{"model":"kiro/sonnet"}]}`},
		{"empty model", `{}`, []string{""}, `null`},
		{"empty preserves picker", `{"env":{"ANTHROPIC_MODEL":"old"},"modelPicker":{"options":[{"model":"kiro/sonnet"}]}}`, []string{""},
			`{"options":[{"model":"kiro/sonnet"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolate(t)
			tool := &ClaudeTool{}
			registry := NewRegistry()
			path := tool.configPath(home)
			writeTestFile(t, path, tt.seed)
			for _, model := range tt.models {
				settings := testSettings
				settings.Model = model
				if _, err := registry.Configure(context.Background(), tool, home, settings); err != nil {
					t.Fatal(err)
				}
				first := readTestFile(t, path)
				if _, err := registry.Configure(context.Background(), tool, home, settings); err != nil {
					t.Fatal(err)
				}
				if got := readTestFile(t, path); got != first {
					t.Fatal("repeated save changed settings or added a duplicate")
				}
				got, err := tool.Read(home)
				if err != nil || got == nil || *got != settings {
					t.Fatalf("readback = %+v, error = %v; want %+v", got, err, settings)
				}
			}
			cfg, _, err := loadJSONObject(path)
			if err != nil {
				t.Fatal(err)
			}
			var want any
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg["modelPicker"], want) {
				t.Fatalf("picker = %#v; want %#v", cfg["modelPicker"], want)
			}
			var original map[string]any
			if err := json.Unmarshal([]byte(tt.seed), &original); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg["permissions"], original["permissions"]) ||
				str(cfg, "env", "CUSTOM") != str(original, "env", "CUSTOM") {
				t.Fatal("unrelated user settings changed")
			}
			if got := readTestFile(t, backupPath(path)); got != tt.seed {
				t.Fatal("original backup was replaced")
			}
			if want != nil {
				if err := tool.Remove(home); err != nil {
					t.Fatal(err)
				}
				removed, _, err := loadJSONObject(path)
				if err != nil || !reflect.DeepEqual(removed["modelPicker"], want) {
					t.Fatalf("disconnect changed picker preferences: %v", err)
				}
			}
		})
	}
}

func TestClaudeModelPickerInvalidStructureDoesNotWrite(t *testing.T) {
	for _, picker := range []string{
		`null`, `"custom"`, `[]`,
		`{"options":null}`, `{"options":{}}`, `{"options":"bad"}`,
		`{"options":[null]}`, `{"options":["kiro/sonnet"]}`,
		`{"options":[{}]}`, `{"options":[{"model":12}]}`, `{"options":[{"model":""}]}`,
		`{"options":[{"model":"openai/gpt-5"},null]}`,
	} {
		t.Run(picker, func(t *testing.T) {
			home := isolate(t)
			tool := &ClaudeTool{}
			path := tool.configPath(home)
			original := `{"env":{"CUSTOM":"untouched"},"modelPicker":` + picker + `}`
			writeTestFile(t, path, original)
			err := tool.Configure(home, testSettings)
			var parseError *ParseError
			if !errors.As(err, &parseError) || parseError.Path != path {
				t.Fatalf("error = %v; want ParseError for %s", err, path)
			}
			if got := readTestFile(t, path); got != original {
				t.Fatal("invalid configuration was rewritten")
			}
			if _, err := os.Stat(backupPath(path)); !os.IsNotExist(err) {
				t.Fatalf("unexpected backup write: %v", err)
			}
		})
	}
}
