package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePreservesProfileTemplate(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "profiles.json"))
	created, err := store.Create(Profile{
		Name:           "Responses Provider",
		Template:       "responses",
		UpstreamFormat: "openai_responses",
		BaseURL:        "https://api.example.com/v1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	profiles, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("List() returned %d profiles, want 1", len(profiles))
	}
	if profiles[0].ID != created.ID {
		t.Fatalf("List() profile ID = %q, want %q", profiles[0].ID, created.ID)
	}
	if profiles[0].Template != "responses" {
		t.Fatalf("List() template = %q, want responses", profiles[0].Template)
	}
}

func TestNormalizeAddsReasoningEffortDefaults(t *testing.T) {
	profile := Normalize(Profile{
		DefaultModel: "grok-4.5",
		Models:       []ModelDef{{Name: "grok-4.5", Model: "grok-4.5"}},
	})
	if profile.DefaultReasoningEffort != "high" {
		t.Fatalf("DefaultReasoningEffort = %q", profile.DefaultReasoningEffort)
	}
	model := profile.Models[0]
	if !model.SupportsReasoningEffort {
		t.Fatal("SupportsReasoningEffort should default to true")
	}
	want := []string{"low", "medium", "high"}
	if !stringSlicesEqual(model.ReasoningEfforts, want) {
		t.Fatalf("ReasoningEfforts = %#v, want %#v", model.ReasoningEfforts, want)
	}
}

func TestStoreRecoversCorruptProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles, err := NewStore(path).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("profiles = %#v, want empty recovery", profiles)
	}
	matches, err := filepath.Glob(path + ".corrupt-*.bak")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt backups = %#v, err = %v", matches, err)
	}
}

func TestStoreReturnsIndependentNestedProfileValues(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "profiles.json"))
	created, err := store.Create(Profile{
		Name:            "provider",
		AvailableModels: []string{"model-a"},
		Models: []ModelDef{{
			Name: "model-a", Model: "model-a",
			ExtraHeaders:     map[string]string{"X-Test": "original"},
			ReasoningEfforts: []string{"low", "high"},
		}},
		ImageGeneration: &ImageGenerationConfig{Enabled: true, AvailableModels: []string{"image-a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	listed[0].AvailableModels[0] = "changed"
	listed[0].Models[0].ExtraHeaders["X-Test"] = "changed"
	listed[0].Models[0].ReasoningEfforts[0] = "changed"
	listed[0].ImageGeneration.AvailableModels[0] = "changed"

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AvailableModels[0] != "model-a" || got.Models[0].ExtraHeaders["X-Test"] != "original" ||
		got.Models[0].ReasoningEfforts[0] != "low" || got.ImageGeneration.AvailableModels[0] != "image-a" {
		t.Fatalf("mutating List result changed cached profile: %#v", got)
	}
}
