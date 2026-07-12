package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"grok_switch/internal/profiles"
)

func TestApplyProfilePreservesUnrelatedSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := `
# keep this comment
[cli]
installer = "local"

[features]
codebase_indexing = true

[endpoints]
models_base_url = "https://old.example/v1"

[models]
default = "old-default"
web_search = "old-search"

[subagents]
default_model = "old-agent"

[ui]
yolo = false
# keep ui comment

[model."old"]
model = "old"
api_key = "old-key"
context_window = 100
max_completion_tokens = 10
max_turns = 2
`
	if err := os.WriteFile(path, []byte(strings.TrimSpace(original)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := profiles.Profile{
		BaseURL:               "https://new.example/v1",
		DefaultModel:          "new-default",
		WebSearchModel:        "new-search",
		SubagentsDefaultModel: "new-agent",
		Models: []profiles.ModelDef{{
			Name:                  "new",
			Model:                 "new-model",
			APIKey:                "new-key",
			APIBackend:            "chat_completions",
			SupportsBackendSearch: true,
			ContextWindow:         200,
			MaxCompletionTokens:   20,
		}},
	}
	if err := ApplyProfileToFile(path, profile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if tableAt(doc, "cli")["installer"] != "local" {
		t.Fatalf("cli section was not preserved: %#v", tableAt(doc, "cli"))
	}
	if tableAt(doc, "features")["codebase_indexing"] != true {
		t.Fatalf("features section was not preserved: %#v", tableAt(doc, "features"))
	}
	if tableAt(doc, "ui")["yolo"] != false {
		t.Fatalf("ui section was not preserved: %#v", tableAt(doc, "ui"))
	}
	if !strings.Contains(string(data), "# keep this comment") || !strings.Contains(string(data), "# keep ui comment") {
		t.Fatalf("unrelated comments were not preserved:\n%s", string(data))
	}
	if tableAt(doc, "endpoints")["models_base_url"] != profile.BaseURL {
		t.Fatalf("base url was not replaced: %#v", tableAt(doc, "endpoints"))
	}
	modelTable := tableAt(doc, "model")
	if _, ok := modelTable["old"]; ok {
		t.Fatalf("old model table still exists: %#v", modelTable)
	}
	if _, ok := modelTable["new"]; !ok {
		t.Fatalf("new model table missing: %#v", modelTable)
	}
	newModel, _ := modelTable["new"].(map[string]any)
	if newModel["api_backend"] != "chat_completions" {
		t.Fatalf("api_backend not written: %#v", newModel)
	}
	if newModel["supports_backend_search"] != true {
		t.Fatalf("supports_backend_search not written: %#v", newModel)
	}
	if _, ok := newModel["max_turns"]; ok {
		t.Fatalf("max_turns should not be written: %#v", newModel)
	}
	if newModel["context_window"] != int64(200) && newModel["context_window"] != int(200) && newModel["context_window"] != float64(200) {
		// go-toml may decode as int64
		if v, ok := newModel["context_window"].(int64); !ok || v != 200 {
			if v2, ok2 := newModel["context_window"].(int); !ok2 || v2 != 200 {
				t.Fatalf("context_window should be written when > 0: %#v", newModel["context_window"])
			}
		}
	}
}

func TestUseOfficialAuthTextRemovesProviderOverrides(t *testing.T) {
	original := `
# keep this comment
[cli]
installer = "local"

[endpoints]
models_base_url = "https://provider.example/v1"
image_base_url = "https://images.example/v1"

[models]
default = "provider-default"
web_search = "provider-search"
temperature = 0.4

[subagents]
enabled = true
default_model = "provider-agent"

[ui]
yolo = false

[model."provider-default"]
model = "provider-model"
api_key = "secret"
base_url = "https://provider.example/v1"
`
	data := UseOfficialAuthText([]byte(strings.TrimSpace(original) + "\n"))
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := tableAt(doc, "endpoints")["models_base_url"]; ok {
		t.Fatalf("models_base_url was not removed:\n%s", string(data))
	}
	if tableAt(doc, "endpoints")["image_base_url"] != "https://images.example/v1" {
		t.Fatalf("unrelated endpoint was not preserved: %#v", tableAt(doc, "endpoints"))
	}
	if _, ok := tableAt(doc, "models")["default"]; ok {
		t.Fatalf("default model was not removed: %#v", tableAt(doc, "models"))
	}
	if _, ok := tableAt(doc, "models")["web_search"]; ok {
		t.Fatalf("web search model was not removed: %#v", tableAt(doc, "models"))
	}
	if tableAt(doc, "models")["temperature"] != 0.4 {
		t.Fatalf("unrelated model default was not preserved: %#v", tableAt(doc, "models"))
	}
	if _, ok := tableAt(doc, "subagents")["default_model"]; ok {
		t.Fatalf("subagent default was not removed: %#v", tableAt(doc, "subagents"))
	}
	if tableAt(doc, "subagents")["enabled"] != true {
		t.Fatalf("subagents enabled flag was not preserved: %#v", tableAt(doc, "subagents"))
	}
	if len(tableAt(doc, "model")) != 0 {
		t.Fatalf("custom model sections were not removed: %#v", tableAt(doc, "model"))
	}
	if tableAt(doc, "ui")["yolo"] != false || !strings.Contains(string(data), "# keep this comment") {
		t.Fatalf("unrelated configuration was not preserved:\n%s", string(data))
	}
}

func TestApplyPrivacyProtectionTextPreservesOtherSettings(t *testing.T) {
	original := `
[features]
telemetry = true
feedback = true

[telemetry]
trace_upload = true
events_url = "https://telemetry.example/events"

[ui]
yolo = false
`
	data := ApplyPrivacyProtectionText([]byte(strings.TrimSpace(original) + "\n"))
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if tableAt(doc, "features")["telemetry"] != false {
		t.Fatalf("telemetry feature was not disabled: %#v", tableAt(doc, "features"))
	}
	if tableAt(doc, "features")["feedback"] != true {
		t.Fatalf("unrelated feature was not preserved: %#v", tableAt(doc, "features"))
	}
	telemetry := tableAt(doc, "telemetry")
	if telemetry["trace_upload"] != false || telemetry["mixpanel_enabled"] != false {
		t.Fatalf("telemetry privacy settings were not applied: %#v", telemetry)
	}
	if telemetry["events_url"] != "https://telemetry.example/events" {
		t.Fatalf("unrelated telemetry setting was not preserved: %#v", telemetry)
	}
	if tableAt(doc, "harness")["disable_codebase_upload"] != true {
		t.Fatalf("harness privacy setting was not applied: %#v", tableAt(doc, "harness"))
	}
	if tableAt(doc, "ui")["yolo"] != false {
		t.Fatalf("unrelated section was not preserved: %#v", tableAt(doc, "ui"))
	}
}

func TestApplyProfileOmitsZeroTokenLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
[endpoints]
models_base_url = "https://old.example/v1"
[models]
default = "m"
web_search = "m"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := profiles.Profile{
		BaseURL:               "https://new.example/v1",
		DefaultModel:          "m",
		WebSearchModel:        "m",
		SubagentsDefaultModel: "m",
		Models: []profiles.ModelDef{{
			Name:                "m",
			Model:               "m",
			APIKey:              "k",
			APIBackend:          "chat_completions",
			ContextWindow:       0,
			MaxCompletionTokens: 0,
		}},
	}
	if err := ApplyProfileToFile(path, profile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "context_window") {
		t.Fatalf("context_window must be omitted when 0:\n%s", text)
	}
	if strings.Contains(text, "max_completion_tokens") {
		t.Fatalf("max_completion_tokens must be omitted when 0:\n%s", text)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	entry, _ := tableAt(doc, "model")["m"].(map[string]any)
	if _, ok := entry["context_window"]; ok {
		t.Fatalf("context_window present in map: %#v", entry)
	}
	if _, ok := entry["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens present in map: %#v", entry)
	}
}

func TestImportProfileAcceptsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`
[endpoints]
models_base_url = "https://old.example/v1"

[models]
default = "old-default"
web_search = "old-search"

[subagents]
default_model = "old-agent"

[model."old"]
model = "old"
api_key = "old-key"
context_window = 100
max_completion_tokens = 10
max_turns = 2
`)...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := ImportProfile(path, "Default")
	if err != nil {
		t.Fatal(err)
	}
	if profile.BaseURL != "https://old.example/v1" || len(profile.Models) != 1 {
		t.Fatalf("unexpected imported profile: %#v", profile)
	}
}

func TestApplyProfileOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
[endpoints]
models_base_url = "https://old.example/v1"

[models]
default = "old-default"
web_search = "old-search"

[subagents]
default_model = "old-agent"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := profiles.Profile{
		BaseURL:               "https://new.example/v1",
		DefaultModel:          "new-default",
		WebSearchModel:        "new-search",
		SubagentsDefaultModel: "new-agent",
	}
	if err := ApplyProfileToFile(path, profile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if tableAt(doc, "endpoints")["models_base_url"] != profile.BaseURL {
		t.Fatalf("base url was not replaced: %#v", tableAt(doc, "endpoints"))
	}
	if tableAt(doc, "models")["default"] != profile.DefaultModel {
		t.Fatalf("default model was not replaced: %#v", tableAt(doc, "models"))
	}
	if tableAt(doc, "models")["web_search"] != profile.WebSearchModel {
		t.Fatalf("web search model was not replaced: %#v", tableAt(doc, "models"))
	}
	if tableAt(doc, "subagents")["default_model"] != profile.SubagentsDefaultModel {
		t.Fatalf("subagents default model was not replaced: %#v", tableAt(doc, "subagents"))
	}
}

func TestCurrentMatchesAfterApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
[endpoints]
models_base_url = "https://old.example/v1"

[models]
default = "old"
web_search = "old"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Profile stores empty per-model base_url/api_key; Apply fills them into config.
	profile := profiles.Profile{
		Name:                  "Test",
		BaseURL:               "https://new.example/v1",
		APIKey:                "sk-test",
		DefaultModel:          "m1",
		WebSearchModel:        "m1",
		SubagentsDefaultModel: "m1",
		Models: []profiles.ModelDef{{
			Name:       "m1",
			Model:      "m1",
			APIBackend: "chat_completions",
		}},
	}
	if err := ApplyProfileToFile(path, profile); err != nil {
		t.Fatal(err)
	}
	ok, err := CurrentMatches(path, profile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected profile to match config after apply (normalized comparison)")
	}
}

func TestCurrentMatchesProfileWithOnlyDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
[endpoints]
models_base_url = "https://api.example/v1"
[models]
default = "x"
web_search = "x"
[subagents]
default_model = "x"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Like the user's "cc" profile: key + defaults, empty models[].
	profile := profiles.Profile{
		Name:                  "cc",
		BaseURL:               "https://api.example/v1",
		APIKey:                "sk-only-on-profile",
		DefaultModel:          "x",
		WebSearchModel:        "x",
		SubagentsDefaultModel: "x",
		Models:                nil,
	}
	if err := ApplyProfileToFile(path, profile); err != nil {
		t.Fatal(err)
	}
	ok, err := CurrentMatches(path, profile)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected defaults-only profile to match after apply")
	}
	// Key must land in config under [model."x"].
	imported, err := ImportProfile(path, "from-file")
	if err != nil {
		t.Fatal(err)
	}
	if imported.EffectiveAPIKey() != "sk-only-on-profile" {
		t.Fatalf("api key not written to config, got %q", imported.EffectiveAPIKey())
	}
}
