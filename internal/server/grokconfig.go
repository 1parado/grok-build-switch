package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"grok_switch/internal/profiles"
)

type grokConfigModelSettings struct {
	Models                 []string            `json:"models"`
	DefaultModel           string              `json:"default_model"`
	DefaultReasoningEffort string              `json:"default_reasoning_effort"`
	ReasoningEfforts       map[string][]string `json:"reasoning_efforts"`
}

func (s *Server) handleGrokConfigModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	configPath := s.Paths.GrokConfig
	if configPath == "" {
		home, _ := os.UserHomeDir()
		grokDir := s.Paths.GrokHome
		if grokDir == "" {
			grokDir = filepath.Join(home, ".grok")
		}
		configPath = filepath.Join(grokDir, "config.toml")
	}

	writeJSON(w, readGrokConfigModelSettings(configPath))
}

func readGrokConfigModels(configPath string) []string {
	return readGrokConfigModelSettings(configPath).Models
}

func readGrokConfigModelSettings(configPath string) grokConfigModelSettings {
	settings := grokConfigModelSettings{ReasoningEfforts: map[string][]string{}}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return settings
	}
	doc := map[string]any{}
	if strings.TrimSpace(string(data)) == "" {
		return settings
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return settings
	}
	if defaults, ok := doc["models"].(map[string]any); ok {
		settings.DefaultModel, _ = defaults["default"].(string)
		settings.DefaultReasoningEffort, _ = defaults["default_reasoning_effort"].(string)
	}
	modelTable, ok := doc["model"].(map[string]any)
	if !ok {
		return settings
	}
	keys := make([]string, 0, len(modelTable))
	for key, raw := range modelTable {
		if profiles.IsMediaModel(profiles.ModelDef{Name: key, Model: key}) {
			continue
		}
		keys = append(keys, key)
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if efforts := stringSliceValue(entry["reasoning_efforts"]); len(efforts) > 0 {
			settings.ReasoningEfforts[key] = efforts
		}
	}
	sort.Strings(keys)
	settings.Models = keys
	return settings
}

func stringSliceValue(value any) []string {
	switch items := value.(type) {
	case []string:
		return append([]string(nil), items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}
