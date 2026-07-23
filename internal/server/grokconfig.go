package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

func (s *Server) handleGrokConfigModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	home, _ := os.UserHomeDir()
	grokDir := s.Paths.GrokHome
	if grokDir == "" {
		grokDir = filepath.Join(home, ".grok")
	}
	configPath := filepath.Join(grokDir, "config.toml")

	models := readGrokConfigModels(configPath)
	writeJSON(w, map[string]any{"models": models})
}

func readGrokConfigModels(configPath string) []string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	doc := map[string]any{}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	modelTable, ok := doc["model"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(modelTable))
	for key := range modelTable {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
