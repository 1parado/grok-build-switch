package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadGrokConfigModelSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[models]
default = "grok-4.5"
default_reasoning_effort = "medium"

[model."gpt-5.6-sol"]
model = "gpt-5.6-sol"
reasoning_efforts = ["low", "high"]

[model."grok-4.5"]
model = "grok-4.5"
reasoning_efforts = ["low", "medium", "high"]

[model."grok-imagine-image"]
model = "grok-imagine-image"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readGrokConfigModelSettings(path)
	if !reflect.DeepEqual(got.Models, []string{"gpt-5.6-sol", "grok-4.5"}) {
		t.Fatalf("Models = %#v", got.Models)
	}
	if got.DefaultModel != "grok-4.5" || got.DefaultReasoningEffort != "medium" {
		t.Fatalf("unexpected defaults: %#v", got)
	}
	if !reflect.DeepEqual(got.ReasoningEfforts["gpt-5.6-sol"], []string{"low", "high"}) {
		t.Fatalf("ReasoningEfforts = %#v", got.ReasoningEfforts)
	}
}
