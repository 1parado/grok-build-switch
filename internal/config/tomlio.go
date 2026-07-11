package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"grok_switch/internal/profiles"
)

func ImportProfile(path, name string) (profiles.Profile, error) {
	doc, err := readDoc(path)
	if err != nil {
		return profiles.Profile{}, err
	}
	profile := profiles.Profile{
		Name:                  name,
		UpstreamFormat:        "openai",
		BaseURL:               stringAt(tableAt(doc, "endpoints"), "models_base_url"),
		DefaultModel:          stringAt(tableAt(doc, "models"), "default"),
		WebSearchModel:        stringAt(tableAt(doc, "models"), "web_search"),
		SubagentsDefaultModel: stringAt(tableAt(doc, "subagents"), "default_model"),
		Models:                readModels(doc),
	}
	if profile.Name == "" {
		profile.Name = "Default"
	}
	return profiles.Normalize(profile), nil
}

func ApplyProfileToFile(path string, profile profiles.Profile) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next, err := ApplyProfileText(data, profile)
	if err != nil {
		return err
	}
	return atomicWrite(path, next)
}

// PreviewApply returns the full config.toml text that would result from
// applying profile onto the existing file (or an empty template if missing).
func PreviewApply(path string, profile profiles.Profile) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		data = []byte{}
	}
	return ApplyProfileText(data, profile)
}

// SnippetForProfile returns only the provider-owned sections as a readable TOML fragment.
func SnippetForProfile(profile profiles.Profile) (string, error) {
	profile = profiles.Normalize(profile)
	var b strings.Builder
	b.WriteString("# 此供应商启用时会写入/覆盖的片段（其它段落保留）\n\n")
	b.WriteString("[endpoints]\n")
	b.WriteString("models_base_url = " + quote(profile.BaseURL) + "\n\n")
	b.WriteString("[models]\n")
	b.WriteString("default = " + quote(profile.DefaultModel) + "\n")
	b.WriteString("web_search = " + quote(profile.WebSearchModel) + "\n\n")
	b.WriteString("[subagents]\n")
	b.WriteString("default_model = " + quote(profile.SubagentsDefaultModel) + "\n\n")
	modelData, err := marshalModelSection(profile)
	if err != nil {
		return "", err
	}
	b.Write(modelData)
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func ApplyProfile(doc map[string]any, profile profiles.Profile) {
	profile = profiles.Normalize(profile)
	endpoints := ensureTable(doc, "endpoints")
	endpoints["models_base_url"] = profile.BaseURL

	models := ensureTable(doc, "models")
	models["default"] = profile.DefaultModel
	models["web_search"] = profile.WebSearchModel

	subagents := ensureTable(doc, "subagents")
	subagents["default_model"] = profile.SubagentsDefaultModel

	modelTable := make(map[string]any, len(profile.Models))
	effectiveKey := profile.EffectiveAPIKey()
	for _, model := range profile.Models {
		key := model.Name
		if key == "" {
			key = model.Model
		}
		apiKey := model.APIKey
		if apiKey == "" {
			apiKey = effectiveKey
		}
		entry := map[string]any{
			"model":                   model.Model,
			"api_key":                 apiKey,
			"api_backend":             model.APIBackend,
			"supports_backend_search": model.SupportsBackendSearch,
		}
		// Omit zero values so Grok uses its own defaults:
		// - omitted context_window → ~200k for new models (or built-in inherit)
		// - omitted max_completion_tokens → global [models] default if set
		if model.ContextWindow > 0 {
			entry["context_window"] = model.ContextWindow
		}
		if model.MaxCompletionTokens > 0 {
			entry["max_completion_tokens"] = model.MaxCompletionTokens
		}
		if model.BaseURL != "" {
			entry["base_url"] = model.BaseURL
		}
		if len(model.ExtraHeaders) > 0 {
			entry["extra_headers"] = model.ExtraHeaders
		}
		modelTable[key] = entry
	}
	doc["model"] = modelTable
}

func ApplyProfileText(data []byte, profile profiles.Profile) ([]byte, error) {
	data = trimUTF8BOM(data)
	newModelData, err := marshalModelSection(profile)
	if err != nil {
		return nil, err
	}
	lines := splitLines(string(data))
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(lines); {
		header := parseHeader(lines[i])
		if header == "" {
			out = append(out, lines[i])
			i++
			continue
		}
		if header == "model" || strings.HasPrefix(header, "model.") {
			i = skipSection(lines, i+1)
			continue
		}
		if header == "endpoints" || header == "models" || header == "subagents" {
			end := skipSection(lines, i+1)
			out = append(out, rewriteSection(lines[i:end], header, profile)...)
			seen[header] = true
			i = end
			continue
		}
		end := skipSection(lines, i+1)
		out = append(out, lines[i:end]...)
		i = end
	}
	for _, section := range []string{"endpoints", "models", "subagents"} {
		if !seen[section] {
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			out = append(out, rewriteSection([]string{"[" + section + "]"}, section, profile)...)
		}
	}
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	out = append(out, strings.TrimRight(string(newModelData), "\r\n"))
	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return []byte(result), nil
}

func CurrentMatches(path string, profile profiles.Profile) (bool, error) {
	current, err := ImportProfile(path, profile.Name)
	if err != nil {
		return false, err
	}
	// Compare normalized views: ApplyProfile fills per-model base_url/api_key
	// into config.toml, while stored profiles may keep those fields empty.
	return profiles.Normalize(profile).Matches(profiles.Normalize(current)), nil
}

func readDoc(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = trimUTF8BOM(data)
	doc := map[string]any{}
	if strings.TrimSpace(string(data)) == "" {
		return doc, nil
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

func readModels(doc map[string]any) []profiles.ModelDef {
	modelTable := tableAt(doc, "model")
	keys := make([]string, 0, len(modelTable))
	for key := range modelTable {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]profiles.ModelDef, 0, len(keys))
	for _, key := range keys {
		table, ok := modelTable[key].(map[string]any)
		if !ok {
			continue
		}
		out = append(out, profiles.ModelDef{
			Name:                  key,
			Model:                 stringAt(table, "model"),
			BaseURL:               stringAt(table, "base_url"),
			APIKey:                stringAt(table, "api_key"),
			APIBackend:            stringAt(table, "api_backend"),
			ExtraHeaders:          stringMapAt(table, "extra_headers"),
			SupportsBackendSearch: boolAt(table, "supports_backend_search"),
			ContextWindow:         intAt(table, "context_window"),
			MaxCompletionTokens:   intAt(table, "max_completion_tokens"),
		})
	}
	return out
}

func marshalModelSection(profile profiles.Profile) ([]byte, error) {
	doc := map[string]any{}
	ApplyProfile(doc, profile)
	delete(doc, "endpoints")
	delete(doc, "models")
	delete(doc, "subagents")
	return toml.Marshal(doc)
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func parseHeader(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return ""
	}
	trimmed = strings.Trim(trimmed, "[]")
	return strings.Trim(trimmed, " ")
}

func skipSection(lines []string, start int) int {
	for start < len(lines) {
		if parseHeader(lines[start]) != "" {
			return start
		}
		start++
	}
	return start
}

func rewriteSection(lines []string, section string, profile profiles.Profile) []string {
	values := map[string]string{}
	switch section {
	case "endpoints":
		values["models_base_url"] = quote(profile.BaseURL)
	case "models":
		values["default"] = quote(profile.DefaultModel)
		values["web_search"] = quote(profile.WebSearchModel)
	case "subagents":
		values["default_model"] = quote(profile.SubagentsDefaultModel)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(lines)+len(values))
	if len(lines) == 0 {
		out = append(out, "["+section+"]")
	} else {
		out = append(out, lines[0])
	}
	for _, line := range lines[1:] {
		key := assignmentKey(line)
		if _, ok := values[key]; ok {
			out = append(out, key+" = "+values[key])
			seen[key] = true
			continue
		}
		out = append(out, line)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+" = "+values[key])
	}
	return out
}

func assignmentKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	idx := strings.Index(trimmed, "=")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(trimmed[:idx])
}

func quote(value string) string {
	var buf bytes.Buffer
	encoder := toml.NewEncoder(&buf)
	_ = encoder.Encode(map[string]string{"x": value})
	line := strings.TrimSpace(buf.String())
	return strings.TrimSpace(strings.TrimPrefix(line, "x = "))
}

func ensureTable(doc map[string]any, key string) map[string]any {
	if table, ok := doc[key].(map[string]any); ok {
		return table
	}
	table := map[string]any{}
	doc[key] = table
	return table
}

func tableAt(doc map[string]any, key string) map[string]any {
	if table, ok := doc[key].(map[string]any); ok {
		return table
	}
	return map[string]any{}
}

func stringAt(table map[string]any, key string) string {
	if v, ok := table[key].(string); ok {
		return v
	}
	return ""
}

func intAt(table map[string]any, key string) int64 {
	switch v := table[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func boolAt(table map[string]any, key string) bool {
	switch v := table[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	case int64:
		return v != 0
	case int:
		return v != 0
	default:
		return false
	}
}

func stringMapAt(table map[string]any, key string) map[string]string {
	raw, ok := table[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch s := v.(type) {
		case string:
			out[k] = s
		case bool:
			if s {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case int64:
			out[k] = fmt.Sprintf("%d", s)
		case int:
			out[k] = fmt.Sprintf("%d", s)
		case float64:
			out[k] = fmt.Sprintf("%v", s)
		default:
			out[k] = fmt.Sprintf("%v", s)
		}
	}
	return out
}

func trimUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return err
			}
			return os.Rename(tmpName, path)
		}
		return err
	}
	return nil
}
