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
	subagents := tableAt(doc, "subagents")
	// [subagents.models] unmarshals as nested table under subagents.models
	subModels := tableAt(subagents, "models")
	explore := stringAt(subModels, "explore")
	plan := stringAt(subModels, "plan")
	// Legacy unrecognized key (kept only for migration on import).
	legacy := stringAt(subagents, "default_model")
	if explore == "" && plan == "" && legacy != "" {
		explore, plan = legacy, legacy
	}
	profile := profiles.Profile{
		Name:                   name,
		UpstreamFormat:         "openai",
		BaseURL:                stringAt(tableAt(doc, "endpoints"), "models_base_url"),
		DefaultModel:           stringAt(tableAt(doc, "models"), "default"),
		DefaultReasoningEffort: stringAt(tableAt(doc, "models"), "default_reasoning_effort"),
		WebSearchModel:         stringAt(tableAt(doc, "models"), "web_search"),
		SubagentsModels: profiles.SubagentsModels{
			Explore: explore,
			Plan:    plan,
		},
		Models: readModels(doc),
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

// UseOfficialAuthToFile removes provider-owned endpoint and model overrides so
// Grok can fall back to the session token managed by `grok login`.
func UseOfficialAuthToFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	next := UseOfficialAuthText(data)
	return atomicWrite(path, next)
}

func UseOfficialAuthText(data []byte) []byte {
	data = trimUTF8BOM(data)
	lines := splitLines(string(data))
	var out []string
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
		end := skipSection(lines, i+1)
		switch header {
		case "endpoints":
			out = append(out, removeAssignments(lines[i:end], "models_base_url")...)
		case "models":
			out = append(out, removeAssignments(lines[i:end], "default", "web_search", "default_reasoning_effort")...)
		case "subagents":
			// Drop legacy default_model; keep enabled and other user keys.
			out = append(out, removeAssignments(lines[i:end], "default_model")...)
		case "subagents.models":
			// Drop switch-managed type model pins so official auth is clean.
			out = append(out, removeAssignments(lines[i:end], "explore", "plan")...)
		default:
			out = append(out, lines[i:end]...)
		}
		i = end
	}
	result := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if result == "" {
		return []byte{}
	}
	return []byte(result + "\n")
}

func ApplyPrivacyProtectionToFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	next := ApplyPrivacyProtectionText(data)
	return atomicWrite(path, next)
}

func ApplyPrivacyProtectionText(data []byte) []byte {
	settings := map[string]map[string]string{
		"features": {
			"telemetry": "false",
		},
		"telemetry": {
			"trace_upload":     "false",
			"mixpanel_enabled": "false",
		},
		"harness": {
			"disable_codebase_upload": "true",
		},
	}
	data = trimUTF8BOM(data)
	lines := splitLines(string(data))
	var out []string
	seen := make(map[string]bool, len(settings))
	for i := 0; i < len(lines); {
		header := parseHeader(lines[i])
		if header == "" {
			out = append(out, lines[i])
			i++
			continue
		}
		end := skipSection(lines, i+1)
		values, ok := settings[header]
		if ok {
			out = append(out, rewriteValues(lines[i:end], values)...)
			seen[header] = true
		} else {
			out = append(out, lines[i:end]...)
		}
		i = end
	}
	for _, section := range []string{"features", "telemetry", "harness"} {
		if seen[section] {
			continue
		}
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, rewriteValues([]string{"[" + section + "]"}, settings[section])...)
	}
	result := strings.TrimRight(strings.Join(out, "\n"), "\n")
	return []byte(result + "\n")
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
	b.WriteString("web_search = " + quote(profile.WebSearchModel) + "\n")
	b.WriteString("default_reasoning_effort = " + quote(profile.DefaultReasoningEffort) + "\n\n")
	if snippet := formatSubagentsModelsSnippet(profile); snippet != "" {
		b.WriteString(snippet)
		if !strings.HasSuffix(snippet, "\n\n") {
			b.WriteString("\n")
		}
	}
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
	models["default_reasoning_effort"] = profile.DefaultReasoningEffort

	applySubagentsModelsToDoc(doc, profile)

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
			"model":                     model.Model,
			"api_key":                   apiKey,
			"api_backend":               model.APIBackend,
			"supports_backend_search":   model.SupportsBackendSearch,
			"supports_reasoning_effort": model.SupportsReasoningEffort,
			"reasoning_efforts":         model.ReasoningEfforts,
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
	profile = profiles.Normalize(profile)
	newModelData, err := marshalModelSection(profile)
	if err != nil {
		return nil, err
	}
	lines := splitLines(string(data))
	var out []string
	seen := map[string]bool{}
	seenSubagentsModels := false
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
		if header == "subagents.models" {
			end := skipSection(lines, i+1)
			if rewritten := rewriteSubagentsModelsSection(lines[i:end], profile); len(rewritten) > 0 {
				out = append(out, rewritten...)
			}
			seenSubagentsModels = true
			i = end
			continue
		}
		if header == "subagents" {
			// Preserve [subagents] keys (e.g. enabled) but drop legacy default_model.
			end := skipSection(lines, i+1)
			out = append(out, removeAssignments(lines[i:end], "default_model")...)
			seen["subagents"] = true
			i = end
			continue
		}
		if header == "endpoints" || header == "models" {
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
	for _, section := range []string{"endpoints", "models"} {
		if !seen[section] {
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			out = append(out, rewriteSection([]string{"[" + section + "]"}, section, profile)...)
		}
	}
	if !seenSubagentsModels {
		if rewritten := rewriteSubagentsModelsSection([]string{"[subagents.models]"}, profile); len(rewritten) > 0 {
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			out = append(out, rewritten...)
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
			Name:                    key,
			Model:                   stringAt(table, "model"),
			BaseURL:                 stringAt(table, "base_url"),
			APIKey:                  stringAt(table, "api_key"),
			APIBackend:              stringAt(table, "api_backend"),
			ExtraHeaders:            stringMapAt(table, "extra_headers"),
			SupportsBackendSearch:   boolAt(table, "supports_backend_search"),
			SupportsReasoningEffort: boolAt(table, "supports_reasoning_effort"),
			ReasoningEfforts:        stringSliceAt(table, "reasoning_efforts"),
			ContextWindow:           intAt(table, "context_window"),
			MaxCompletionTokens:     intAt(table, "max_completion_tokens"),
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

func applySubagentsModelsToDoc(doc map[string]any, profile profiles.Profile) {
	sub := ensureTable(doc, "subagents")
	delete(sub, "default_model")
	models := map[string]any{}
	if profile.SubagentsModels.Explore != "" {
		models["explore"] = profile.SubagentsModels.Explore
	}
	if profile.SubagentsModels.Plan != "" {
		models["plan"] = profile.SubagentsModels.Plan
	}
	if len(models) > 0 {
		sub["models"] = models
	} else {
		delete(sub, "models")
	}
	if len(sub) == 0 {
		delete(doc, "subagents")
	}
}

func formatSubagentsModelsSnippet(profile profiles.Profile) string {
	lines := rewriteSubagentsModelsSection([]string{"[subagents.models]"}, profile)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n\n"
}

// rewriteSubagentsModelsSection rewrites or creates [subagents.models].
// Empty explore/plan means omit that key. If both empty, the section is removed.
func rewriteSubagentsModelsSection(lines []string, profile profiles.Profile) []string {
	values := map[string]string{}
	if strings.TrimSpace(profile.SubagentsModels.Explore) != "" {
		values["explore"] = quote(strings.TrimSpace(profile.SubagentsModels.Explore))
	}
	if strings.TrimSpace(profile.SubagentsModels.Plan) != "" {
		values["plan"] = quote(strings.TrimSpace(profile.SubagentsModels.Plan))
	}
	if len(values) == 0 {
		return nil
	}
	managed := map[string]bool{"explore": true, "plan": true}
	seen := map[string]bool{}
	out := make([]string, 0, len(lines)+len(values))
	if len(lines) == 0 {
		out = append(out, "[subagents.models]")
	} else {
		out = append(out, "[subagents.models]")
	}
	start := 1
	if len(lines) > 0 && parseHeader(lines[0]) == "" {
		start = 0
	}
	for _, line := range lines[start:] {
		key := assignmentKey(line)
		if managed[key] {
			if val, ok := values[key]; ok {
				out = append(out, key+" = "+val)
				seen[key] = true
			}
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
		values["default_reasoning_effort"] = quote(profile.DefaultReasoningEffort)
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

func removeAssignments(lines []string, keys ...string) []string {
	removed := make(map[string]bool, len(keys))
	for _, key := range keys {
		removed[key] = true
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !removed[assignmentKey(line)] {
			out = append(out, line)
		}
	}
	return out
}

func rewriteValues(lines []string, values map[string]string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(lines)+len(values))
	if len(lines) == 0 {
		return out
	}
	out = append(out, lines[0])
	for _, line := range lines[1:] {
		key := assignmentKey(line)
		value, ok := values[key]
		if ok {
			out = append(out, key+" = "+value)
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

func stringSliceAt(table map[string]any, key string) []string {
	raw, ok := table[key].([]any)
	if !ok {
		if values, stringsOK := table[key].([]string); stringsOK {
			return append([]string(nil), values...)
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, textOK := value.(string); textOK && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
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
