package profiles

import (
	"strings"
	"time"
)

type ModelDef struct {
	Name                    string            `json:"name"`
	DisplayName             string            `json:"display_name,omitempty"`
	Model                   string            `json:"model"`
	BaseURL                 string            `json:"base_url"`
	APIKey                  string            `json:"api_key"`
	APIBackend              string            `json:"api_backend"`
	ExtraHeaders            map[string]string `json:"extra_headers"`
	SupportsBackendSearch   bool              `json:"supports_backend_search"`
	SupportsReasoningEffort bool              `json:"supports_reasoning_effort"`
	ReasoningEfforts        []string          `json:"reasoning_efforts"`
	ContextWindow           int64             `json:"context_window"`
	MaxCompletionTokens     int64             `json:"max_completion_tokens"`
}

// SubagentsModels maps built-in subagent types to model IDs written under
// [subagents.models] in Grok config.toml.
type SubagentsModels struct {
	Explore string `json:"explore,omitempty"`
	Plan    string `json:"plan,omitempty"`
}

// ImageGenerationConfig configures Grok's built-in /imagine command. It is
// deliberately independent from the chat provider connection.
type ImageGenerationConfig struct {
	Enabled         bool     `json:"enabled"`
	BaseURL         string   `json:"base_url"`
	APIKey          string   `json:"api_key"`
	APIBackend      string   `json:"api_backend"`
	Model           string   `json:"model"`
	AvailableModels []string `json:"available_models,omitempty"`
}

type Profile struct {
	ID                     string                 `json:"id"`
	Name                   string                 `json:"name"`
	Template               string                 `json:"template,omitempty"`
	UpstreamFormat         string                 `json:"upstream_format"`
	BaseURL                string                 `json:"base_url"`
	APIKey                 string                 `json:"api_key"`
	AvailableModels        []string               `json:"available_models"`
	DefaultModel           string                 `json:"default_model"`
	DefaultReasoningEffort string                 `json:"default_reasoning_effort"`
	WebSearchModel         string                 `json:"web_search_model"`
	SubagentsModels        SubagentsModels        `json:"subagents_models"`
	ImageGeneration        *ImageGenerationConfig `json:"image_generation,omitempty"`
	// Features, MediaModels and FeatureModels are legacy fields migrated into
	// ImageGeneration by Normalize. New profiles do not persist them.
	Features      map[string]bool   `json:"features,omitempty"`
	MediaModels   map[string]string `json:"media_models,omitempty"`
	FeatureModels map[string]string `json:"feature_models,omitempty"`
	// SubagentsDefaultModel is deprecated (legacy profiles / old config key).
	// Normalize migrates a non-empty value into SubagentsModels when explore/plan are empty.
	SubagentsDefaultModel string     `json:"subagents_default_model,omitempty"`
	Models                []ModelDef `json:"models"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	IsActive              bool       `json:"is_active"`
}

func (p Profile) Matches(other Profile) bool {
	p = Normalize(p)
	other = Normalize(other)
	if p.BaseURL != other.BaseURL ||
		!runtimeDefaultMatches(p, other.DefaultModel) ||
		p.DefaultReasoningEffort != other.DefaultReasoningEffort ||
		p.WebSearchModel != other.WebSearchModel ||
		p.SubagentsModels.Explore != other.SubagentsModels.Explore ||
		p.SubagentsModels.Plan != other.SubagentsModels.Plan ||
		!imageGenerationMatches(p.ImageGeneration, other.ImageGeneration) {
		return false
	}
	// config.toml only stores keys on [model.*] entries. A profile with no
	// enabled models cannot persist its profile-level api_key, so skip key
	// comparison when both sides have zero model definitions.
	if len(p.Models) == 0 && len(other.Models) == 0 {
		return true
	}
	if effectiveAPIKey(p) != effectiveAPIKey(other) || len(p.Models) != len(other.Models) {
		return false
	}
	byName := make(map[string]ModelDef, len(p.Models))
	for _, model := range p.Models {
		byName[modelKey(model)] = model
	}
	for _, model := range other.Models {
		stored, ok := byName[modelKey(model)]
		if !ok {
			return false
		}
		if !modelEqual(stored, model) {
			return false
		}
	}
	return true
}

func runtimeDefaultMatches(profile Profile, actual string) bool {
	if profile.DefaultModel == actual {
		return true
	}
	for _, model := range profile.Models {
		if modelKey(model) != actual {
			continue
		}
		return !IsMediaModel(model)
	}
	return false
}

func modelKey(m ModelDef) string {
	if m.Name != "" {
		return m.Name
	}
	return m.Model
}

func modelEqual(a, b ModelDef) bool {
	if modelKey(a) != modelKey(b) ||
		a.Model != b.Model ||
		a.DisplayName != b.DisplayName ||
		a.BaseURL != b.BaseURL ||
		a.APIKey != b.APIKey ||
		a.APIBackend != b.APIBackend ||
		a.SupportsBackendSearch != b.SupportsBackendSearch ||
		a.SupportsReasoningEffort != b.SupportsReasoningEffort ||
		a.ContextWindow != b.ContextWindow ||
		a.MaxCompletionTokens != b.MaxCompletionTokens {
		return false
	}
	if !stringSlicesEqual(a.ReasoningEfforts, b.ReasoningEfforts) {
		return false
	}
	if len(a.ExtraHeaders) != len(b.ExtraHeaders) {
		return false
	}
	for k, v := range a.ExtraHeaders {
		if b.ExtraHeaders[k] != v {
			return false
		}
	}
	return true
}

func (p Profile) EffectiveAPIKey() string {
	return effectiveAPIKey(p)
}

func Normalize(p Profile) Profile {
	if p.DefaultReasoningEffort == "" {
		p.DefaultReasoningEffort = "high"
	}
	if p.UpstreamFormat == "" {
		p.UpstreamFormat = "openai_chat"
	}
	if p.UpstreamFormat == "openai" || p.UpstreamFormat == "grok" {
		p.UpstreamFormat = "openai_chat"
	}
	if p.APIKey == "" {
		p.APIKey = effectiveAPIKey(p)
	}
	// Migrate legacy single subagents_default_model into per-type fields.
	if p.SubagentsModels.Explore == "" && p.SubagentsModels.Plan == "" && p.SubagentsDefaultModel != "" {
		p.SubagentsModels.Explore = p.SubagentsDefaultModel
		p.SubagentsModels.Plan = p.SubagentsDefaultModel
	}
	p.SubagentsDefaultModel = ""
	p.ImageGeneration = migrateImageGeneration(p)
	legacyImageModel := strings.TrimSpace(p.MediaModels["grok-imagine-image"])
	if legacyImageModel == "" {
		legacyImageModel = strings.TrimSpace(p.FeatureModels["image_gen"])
	}
	p.Features = nil
	p.MediaModels = nil
	p.FeatureModels = nil
	if IsMediaModel(ModelDef{Name: p.DefaultModel, Model: p.DefaultModel}) || p.DefaultModel == legacyImageModel {
		p.DefaultModel = ""
	}
	if IsMediaModel(ModelDef{Name: p.WebSearchModel, Model: p.WebSearchModel}) || p.WebSearchModel == legacyImageModel {
		p.WebSearchModel = ""
	}
	if IsMediaModel(ModelDef{Name: p.SubagentsModels.Explore, Model: p.SubagentsModels.Explore}) || p.SubagentsModels.Explore == legacyImageModel {
		p.SubagentsModels.Explore = ""
	}
	if IsMediaModel(ModelDef{Name: p.SubagentsModels.Plan, Model: p.SubagentsModels.Plan}) || p.SubagentsModels.Plan == legacyImageModel {
		p.SubagentsModels.Plan = ""
	}
	// Profiles with only default model names (no models[]) still need a
	// writable [model.*] entry so config.toml can store the API key.
	if len(p.Models) == 0 {
		names := uniqueStrings([]string{
			p.DefaultModel,
			p.WebSearchModel,
			p.SubagentsModels.Explore,
			p.SubagentsModels.Plan,
		})
		for _, name := range names {
			if name == "" {
				continue
			}
			p.Models = append(p.Models, ModelDef{
				Name:  name,
				Model: name,
			})
		}
	}
	normalizedModels := make([]ModelDef, 0, len(p.Models))
	for i := range p.Models {
		if p.Models[i].Name == "" {
			p.Models[i].Name = p.Models[i].Model
		}
		if p.Models[i].Model == "" {
			p.Models[i].Model = p.Models[i].Name
		}
		if p.Models[i].BaseURL == "" {
			p.Models[i].BaseURL = p.BaseURL
		}
		if p.Models[i].APIKey == "" {
			p.Models[i].APIKey = p.APIKey
		}
		if p.Models[i].APIBackend == "" {
			p.Models[i].APIBackend = APIBackendForUpstreamFormat(p.UpstreamFormat)
		}
		if p.Models[i].ExtraHeaders == nil {
			p.Models[i].ExtraHeaders = map[string]string{}
		}
		if IsMediaModel(p.Models[i]) || (legacyImageModel != "" && (modelKey(p.Models[i]) == legacyImageModel || p.Models[i].Model == legacyImageModel)) {
			continue
		}
		p.Models[i].SupportsReasoningEffort = true
		if len(p.Models[i].ReasoningEfforts) == 0 {
			p.Models[i].ReasoningEfforts = []string{"low", "medium", "high"}
		} else {
			p.Models[i].ReasoningEfforts = uniqueStrings(p.Models[i].ReasoningEfforts)
		}
		normalizedModels = append(normalizedModels, p.Models[i])
	}
	p.Models = normalizedModels
	p.AvailableModels = uniqueStrings(p.AvailableModels)
	return p
}

func IsMediaModel(model ModelDef) bool {
	id := strings.ToLower(strings.TrimSpace(model.Model))
	if id == "" {
		id = strings.ToLower(strings.TrimSpace(model.Name))
	}
	return id == "grok-imagine-image" || id == "grok-imagine-image-quality" || id == "grok-imagine-video"
}

func migrateImageGeneration(p Profile) *ImageGenerationConfig {
	if p.ImageGeneration != nil {
		config := *p.ImageGeneration
		return normalizeImageGeneration(&config)
	}
	if !p.Features["image_gen"] {
		return nil
	}
	selected := strings.TrimSpace(p.MediaModels["grok-imagine-image"])
	if selected == "" {
		selected = strings.TrimSpace(p.FeatureModels["image_gen"])
	}
	if selected == "" {
		selected = "grok-imagine-image"
	}
	config := &ImageGenerationConfig{Enabled: true, Model: selected}
	for _, model := range p.Models {
		if modelKey(model) != selected && model.Model != selected && model.Name != "grok-imagine-image" {
			continue
		}
		config.Model = model.Model
		config.BaseURL = model.BaseURL
		config.APIKey = model.APIKey
		config.APIBackend = model.APIBackend
		break
	}
	if config.BaseURL == "" {
		config.BaseURL = p.BaseURL
	}
	if config.APIKey == "" {
		config.APIKey = p.APIKey
	}
	return normalizeImageGeneration(config)
}

func normalizeImageGeneration(config *ImageGenerationConfig) *ImageGenerationConfig {
	if config == nil {
		return nil
	}
	config.BaseURL = strings.TrimSpace(config.BaseURL)
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	switch config.APIBackend {
	case "responses", "messages", "chat_completions":
	default:
		config.APIBackend = "chat_completions"
	}
	config.AvailableModels = uniqueStrings(config.AvailableModels)
	return config
}

func imageGenerationMatches(expected, actual *ImageGenerationConfig) bool {
	if expected == nil || !expected.Enabled {
		return actual == nil || !actual.Enabled
	}
	if actual == nil || !actual.Enabled {
		return false
	}
	return expected.BaseURL == actual.BaseURL && expected.APIKey == actual.APIKey &&
		expected.APIBackend == actual.APIBackend && expected.Model == actual.Model
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func APIBackendForUpstreamFormat(upstreamFormat string) string {
	switch upstreamFormat {
	case "openai_responses", "responses":
		return "responses"
	case "anthropic", "messages":
		return "messages"
	case "openai_chat", "openai", "grok", "custom", "chat_completions":
		return "chat_completions"
	default:
		return "chat_completions"
	}
}

func effectiveAPIKey(p Profile) string {
	if p.APIKey != "" {
		return p.APIKey
	}
	for _, model := range p.Models {
		if model.APIKey != "" {
			return model.APIKey
		}
	}
	return ""
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
