package profiles

import "testing"

func TestNormalizeMigratesLegacyImageModelToIndependentConfig(t *testing.T) {
	profile := Normalize(Profile{
		BaseURL:         "https://chat.example/v1",
		APIKey:          "sk-chat",
		DefaultModel:    "vendor-image-v2",
		WebSearchModel:  "vendor-image-v2",
		SubagentsModels: SubagentsModels{Explore: "vendor-image-v2", Plan: "vendor-image-v2"},
		Features:        map[string]bool{"image_gen": true, "image_edit": true, "video_gen": true},
		MediaModels:     map[string]string{"grok-imagine-image": "vendor-image-v2", "grok-imagine-video": "vendor-video"},
		Models: []ModelDef{{
			Name: "vendor-image-v2", Model: "vendor-image-v2", BaseURL: "https://image.example/v1",
			APIKey: "sk-image", APIBackend: "responses",
		}},
	})
	if profile.ImageGeneration == nil || !profile.ImageGeneration.Enabled {
		t.Fatalf("legacy image config was not migrated: %#v", profile.ImageGeneration)
	}
	image := profile.ImageGeneration
	if image.BaseURL != "https://image.example/v1" || image.APIKey != "sk-image" || image.Model != "vendor-image-v2" || image.APIBackend != "responses" {
		t.Fatalf("migrated image config = %#v", image)
	}
	if len(profile.Features) != 0 || len(profile.MediaModels) != 0 || len(profile.FeatureModels) != 0 {
		t.Fatalf("legacy media fields were retained: %#v", profile)
	}
	if len(profile.Models) != 0 || profile.DefaultModel != "" || profile.WebSearchModel != "" || profile.SubagentsModels.Explore != "" || profile.SubagentsModels.Plan != "" {
		t.Fatalf("legacy image model remained in chat roles: %#v", profile)
	}
}

func TestMatchesComparesIndependentImageConnection(t *testing.T) {
	base := Profile{
		BaseURL: "https://chat.example/v1", APIKey: "sk-chat",
		DefaultModel: "chat", Models: []ModelDef{{Name: "chat", Model: "chat"}},
		ImageGeneration: &ImageGenerationConfig{
			Enabled: true, BaseURL: "https://image.example/v1", APIKey: "sk-image",
			APIBackend: "chat_completions", Model: "image-a",
		},
	}
	other := base
	copy := *base.ImageGeneration
	copy.APIKey = "sk-other"
	other.ImageGeneration = &copy
	if base.Matches(other) {
		t.Fatal("profiles with different image API keys should not match")
	}
}

func TestMatchesAllowsRuntimeSwitchToEnabledChatModel(t *testing.T) {
	base := Profile{
		BaseURL:      "https://api.example.com/v1",
		APIKey:       "sk-test",
		DefaultModel: "chat-a",
		Models: []ModelDef{
			{Name: "chat-a", Model: "chat-a"},
			{Name: "chat-b", Model: "chat-b"},
		},
		ImageGeneration: &ImageGenerationConfig{
			Enabled: true, BaseURL: "https://image.example/v1", APIKey: "sk-image",
			APIBackend: "responses", Model: "image-a",
		},
	}
	switched := base
	switched.DefaultModel = "chat-b"
	if !base.Matches(switched) {
		t.Fatal("switching /model to another enabled chat model should not cause config drift")
	}
	switched.DefaultModel = "unknown"
	if base.Matches(switched) {
		t.Fatal("an unknown default model should still cause config drift")
	}
	switched.DefaultModel = "grok-imagine-image"
	if base.Matches(switched) {
		t.Fatal("the built-in image alias should not become the chat default")
	}
}
