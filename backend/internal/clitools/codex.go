package clitools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// CodexTool auto-configures Codex CLI (~/.codex/config.toml).
//
// The key is stored as the provider's experimental_bearer_token rather than in
// auth.json: auth.json holds the user's own OpenAI / ChatGPT login, and
// overwriting it would sign them out of Codex.
type CodexTool struct{}

func (t *CodexTool) ID() string                    { return "codex" }
func (t *CodexTool) Name() string                  { return "Codex CLI" }
func (t *CodexTool) Commands() []string            { return []string{"codex"} }
func (t *CodexTool) Extensions() []string          { return []string{"openai.chatgpt"} }
func (t *CodexTool) ModelRequired() bool           { return false }
func (t *CodexTool) ConfigFiles(h string) []string { return []string{t.configPath(h)} }

// codexDir honors CODEX_HOME, Codex's override for ~/.codex.
func (t *CodexTool) codexDir(homeDir string) string {
	return envDir(homeDir, "CODEX_HOME", "~/.codex")
}

func (t *CodexTool) configPath(homeDir string) string {
	return filepath.Join(t.codexDir(homeDir), "config.toml")
}

func (t *CodexTool) catalogPath(homeDir string) string {
	return filepath.Join(t.codexDir(homeDir), codexCatalogFilename)
}

const (
	codexProviderID      = "keirouter"
	codexCatalogFilename = "kiro-gateway-models.json"
)

func (t *CodexTool) Read(homeDir string) (*Settings, error) {
	cfg, exists, err := loadTOML(t.configPath(homeDir))
	if err != nil || !exists {
		return nil, err
	}
	// The provider block is inert unless it is the selected provider.
	if str(cfg, "model_provider") != codexProviderID {
		return nil, nil
	}
	base := str(cfg, "model_providers", codexProviderID, "base_url")
	if base == "" {
		return nil, nil
	}
	return &Settings{
		BaseURL: base,
		APIKey:  str(cfg, "model_providers", codexProviderID, "experimental_bearer_token"),
		Model:   str(cfg, "model"),
	}, nil
}

func (t *CodexTool) Configure(homeDir string, s Settings) error {
	path := t.configPath(homeDir)
	cfg, _, err := loadTOML(path)
	if err != nil {
		return err
	}

	catalogPath, catalogDoc, writeCatalog, err := t.prepareCatalogSync(homeDir, path, cfg, s)
	if err != nil {
		return err
	}

	providers := child(cfg, "model_providers")
	if s.ReplaceCodexProviders {
		replacing := len(providers) > 1 || (len(providers) == 1 && providers[codexProviderID] == nil)
		if replacing {
			// Keep the exact pre-takeover config, even if the first-connect backup is older.
			data, _, err := readFile(path)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(path+".before-kiro-takeover.bak", data); err != nil {
				return err
			}
			profiles, _ := cfg["profiles"].(map[string]any)
			for _, value := range profiles {
				profile, _ := value.(map[string]any)
				id := str(profile, "model_provider")
				if _, removed := providers[id]; removed && id != codexProviderID {
					delete(profile, "model_provider")
					delete(profile, "model")
				}
			}
			clear(providers)
		}
	}

	if writeCatalog {
		if fileExists(catalogPath) && !fileExists(backupPath(catalogPath)) {
			if err := backupFile(catalogPath); err != nil {
				return err
			}
		}
		if err := saveJSON(catalogPath, catalogDoc); err != nil {
			return err
		}
	}
	if catalogPath != "" && strings.TrimSpace(str(cfg, "model_catalog_json")) == "" {
		cfg["model_catalog_json"] = catalogPath
	}
	if s.CodexModel != nil && s.CodexModel.EffortKnown {
		if curEffort := str(cfg, "model_reasoning_effort"); curEffort != "" && !slices.Contains(s.CodexModel.EffortLevels, curEffort) {
			delete(cfg, "model_reasoning_effort")
		}
	}

	if s.Model != "" {
		cfg["model"] = s.Model
	}
	cfg["model_provider"] = codexProviderID
	providers[codexProviderID] = map[string]any{
		"name":                      "Kiro Gateway",
		"base_url":                  v1URL(s.BaseURL),
		"wire_api":                  "responses",
		"experimental_bearer_token": s.APIKey,
	}
	return saveTOML(path, cfg)
}

func (t *CodexTool) prepareCatalogSync(homeDir, configPath string, cfg map[string]any, s Settings) (string, map[string]any, bool, error) {
	var existingCatalog string
	if raw, exists := cfg["model_catalog_json"]; exists {
		strVal, ok := raw.(string)
		if !ok {
			return "", nil, false, &ParseError{Path: configPath, Err: fmt.Errorf("model_catalog_json must be a string")}
		}
		existingCatalog = strings.TrimSpace(strVal)
	}
	if s.Model == "" || (!strings.HasPrefix(s.Model, "kiro/") && !s.ReplaceCodexProviders && existingCatalog == "") {
		return "", nil, false, nil
	}

	catalogPath := t.catalogPath(homeDir)
	if existingCatalog != "" {
		if filepath.IsAbs(existingCatalog) {
			catalogPath = filepath.Clean(existingCatalog)
		} else {
			catalogPath = filepath.Join(t.codexDir(homeDir), existingCatalog)
		}
	}

	doc, exists, err := loadStrictJSONObject(catalogPath)
	if err != nil {
		return "", nil, false, err
	}
	changed, err := ensureCodexModelCatalog(doc, s.Model, s.CodexModel)
	if err != nil {
		return "", nil, false, &ParseError{Path: catalogPath, Err: err}
	}
	return catalogPath, doc, !exists || changed, nil
}

func loadStrictJSONObject(path string) (map[string]any, bool, error) {
	data, exists, err := readFile(path)
	if err != nil || !exists || isBlank(data) {
		return map[string]any{}, exists, err
	}
	var raw any
	if err := json.Unmarshal(stripJSONC(data), &raw); err != nil {
		return nil, true, &ParseError{Path: path, Err: err}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, true, &ParseError{Path: path, Err: fmt.Errorf("catalog root must be a JSON object")}
	}
	return obj, true, nil
}

func ensureCodexModelCatalog(doc map[string]any, slug string, meta *CodexModelMetadata) (bool, error) {
	var rawModels []any
	_, hadModelsKey := doc["models"]
	if hadModelsKey {
		var ok bool
		rawModels, ok = doc["models"].([]any)
		if !ok {
			return false, fmt.Errorf("models must be an array")
		}
	}

	changed := !hadModelsKey
	deduped := make([]any, 0, len(rawModels)+1)
	seen := make(map[string]int, len(rawModels))
	for i, item := range rawModels {
		entry, ok := item.(map[string]any)
		if !ok {
			return false, fmt.Errorf("models[%d] must be an object", i)
		}
		id, ok := entry["slug"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return false, fmt.Errorf("models[%d].slug must be a non-empty string", i)
		}
		if _, dup := seen[id]; dup {
			changed = true
			continue
		}
		seen[id] = len(deduped)
		deduped = append(deduped, entry)
	}

	if idx, exists := seen[slug]; exists {
		entry := deduped[idx].(map[string]any)
		if meta != nil && len(meta.EffortLevels) > 0 && codexReasoningLevelsEmpty(entry["supported_reasoning_levels"]) {
			entry["supported_reasoning_levels"] = buildCodexReasoningLevels(meta.EffortLevels)
			if def, _ := entry["default_reasoning_level"].(string); strings.TrimSpace(def) == "" {
				entry["default_reasoning_level"] = defaultCodexReasoningLevel(meta.EffortLevels)
			}
			changed = true
		}
		doc["models"] = deduped
		return changed, nil
	}

	newEntry := newCodexCatalogModel(slug, meta)
	insertAt := 0
	for i, item := range deduped {
		entry := item.(map[string]any)
		if id, _ := entry["slug"].(string); strings.HasPrefix(id, "kiro/") || isZeroPriority(entry["priority"]) {
			insertAt = i + 1
		}
	}
	deduped = slices.Insert(deduped, insertAt, any(newEntry))
	doc["models"] = deduped
	return true, nil
}

func codexReasoningLevelsEmpty(raw any) bool {
	if raw == nil {
		return true
	}
	arr, ok := raw.([]any)
	return ok && len(arr) == 0
}

func isZeroPriority(raw any) bool {
	switch v := raw.(type) {
	case float64:
		return v == 0
	case int:
		return v == 0
	default:
		return false
	}
}

func newCodexCatalogModel(slug string, meta *CodexModelMetadata) map[string]any {
	displayName := defaultCodexDisplayName(slug)
	description := defaultCodexDescription(displayName)
	var levels []string
	if meta != nil {
		if strings.TrimSpace(meta.DisplayName) != "" {
			displayName = strings.TrimSpace(meta.DisplayName)
			description = defaultCodexDescription(displayName)
		}
		if strings.TrimSpace(meta.Description) != "" {
			description = strings.TrimSpace(meta.Description)
		}
		levels = meta.EffortLevels
	}

	entry := map[string]any{
		"slug":                         slug,
		"display_name":                 displayName,
		"description":                  description,
		"visibility":                   "list",
		"supported_in_api":             true,
		"priority":                     0,
		"supported_reasoning_levels":   buildCodexReasoningLevels(levels),
		"shell_type":                   "unified_exec",
		"base_instructions":            "You are a coding assistant. Help the user with their task using the available tools.",
		"support_verbosity":            false,
		"supports_reasoning_summaries": false,
		"truncation_policy": map[string]any{
			"mode":  "tokens",
			"limit": 10000,
		},
		"experimental_supported_tools": []any{},
		"input_modalities":             []any{"text", "image"},
		"apply_patch_tool_type":        "freeform",
	}
	if len(levels) > 0 {
		entry["default_reasoning_level"] = defaultCodexReasoningLevel(levels)
	}
	return entry
}

func defaultCodexDisplayName(slug string) string {
	if after, ok := strings.CutPrefix(slug, "kiro/"); ok && after != "" {
		return "Kiro " + after
	}
	return slug
}

func defaultCodexDescription(displayName string) string {
	trimmed := strings.TrimPrefix(displayName, "Kiro ")
	if trimmed == "" {
		trimmed = displayName
	}
	return trimmed + " via Kiro Gateway."
}

var codexEffortDescriptions = map[string]string{
	"none":    "No additional reasoning before responding",
	"minimal": "Minimal reasoning for the fastest responses",
	"low":     "Fast responses with lighter reasoning",
	"medium":  "Balances speed and reasoning depth for everyday tasks",
	"high":    "Greater reasoning depth for complex problems",
	"xhigh":   "Extra high reasoning depth for complex problems",
	"max":     "Maximum reasoning depth for the hardest problems",
	"ultra":   "Maximum reasoning with automatic task delegation",
}

func buildCodexReasoningLevels(levels []string) []any {
	out := make([]any, 0, len(levels))
	seen := make(map[string]bool, len(levels))
	for _, level := range levels {
		level = strings.TrimSpace(level)
		if level == "" || seen[level] {
			continue
		}
		seen[level] = true
		desc, ok := codexEffortDescriptions[level]
		if !ok {
			desc = "Reasoning effort: " + level
		}
		out = append(out, map[string]any{
			"effort":      level,
			"description": desc,
		})
	}
	return out
}

func defaultCodexReasoningLevel(levels []string) string {
	for _, preferred := range []string{"medium", "high", "low"} {
		if slices.Contains(levels, preferred) {
			return preferred
		}
	}
	return levels[0]
}

func (t *CodexTool) Remove(homeDir string) error {
	path := t.configPath(homeDir)
	cfg, exists, err := loadTOML(path)
	if err != nil || !exists {
		return err
	}
	if str(cfg, "model_provider") == codexProviderID {
		delete(cfg, "model_provider")
		// The model was chosen for the gateway; Codex falls back to its own
		// default without it.
		delete(cfg, "model")
		if existing := strings.TrimSpace(str(cfg, "model_catalog_json")); existing != "" &&
			filepath.Clean(existing) == filepath.Clean(t.catalogPath(homeDir)) {
			delete(cfg, "model_catalog_json")
		}
	}
	if providers, ok := cfg["model_providers"].(map[string]any); ok {
		delete(providers, codexProviderID)
		pruneEmpty(cfg, "model_providers")
	}
	return saveOrRemove(path, cfg, saveTOML)
}
