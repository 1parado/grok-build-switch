package config

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"grok_switch/internal/profiles"
)

func TestApplyProfileKeepsSectionWhenHeaderHasComment(t *testing.T) {
	original := `
[endpoints]
models_base_url = "https://old.example/v1"
keep_me = true

[models] # user note
default = "old-default"
custom = true
`
	profile := profiles.Profile{
		BaseURL:      "https://new.example/v1",
		DefaultModel: "new-default",
		APIKey:       "k",
	}
	out, err := ApplyProfileText([]byte(original), profile)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten config does not parse: %v\n%s", err, out)
	}
	models := tableAt(doc, "models")
	if models["default"] != "new-default" {
		t.Fatalf("default = %#v\n%s", models["default"], out)
	}
	if models["custom"] != true {
		t.Fatalf("custom key left the [models] table: %#v\n%s", models["custom"], out)
	}
	endpoints := tableAt(doc, "endpoints")
	if endpoints["models_base_url"] != profile.BaseURL {
		t.Fatalf("base url = %#v", endpoints["models_base_url"])
	}
	if endpoints["keep_me"] != true {
		t.Fatalf("endpoints.keep_me = %#v\n%s", endpoints["keep_me"], out)
	}
	if _, ok := endpoints["custom"]; ok {
		t.Fatalf("custom key was absorbed by [endpoints]: %#v\n%s", endpoints, out)
	}
	if !strings.Contains(string(out), "[models] # user note") {
		t.Fatalf("header comment was dropped:\n%s", out)
	}
}

func TestApplyProfileIgnoresHeaderInsideMultilineString(t *testing.T) {
	original := `
[cli]
note = """
[models]
default = "not-a-key"
"""
keep = 1

[models]
default = "old-default"
custom = true
`
	profile := profiles.Profile{
		BaseURL:      "https://new.example/v1",
		DefaultModel: "new-default",
		APIKey:       "k",
	}
	out, err := ApplyProfileText([]byte(original), profile)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten config does not parse: %v\n%s", err, out)
	}
	cli := tableAt(doc, "cli")
	note, _ := cli["note"].(string)
	if !strings.Contains(note, "[models]") || !strings.Contains(note, "not-a-key") {
		t.Fatalf("multiline note was rewritten: %#v\n%s", cli["note"], out)
	}
	if cli["keep"] != int64(1) && cli["keep"] != 1 {
		t.Fatalf("cli.keep = %#v\n%s", cli["keep"], out)
	}
	models := tableAt(doc, "models")
	if models["default"] != "new-default" || models["custom"] != true {
		t.Fatalf("models table = %#v\n%s", models, out)
	}
}

func TestRemoveMcpServerKeepsWorkingWhenHeaderHasComment(t *testing.T) {
	original := `
[cli]
installer = "local"

[mcp_servers.imagine] # generated
command = "grok-switch"
args = ["imagine"]

[ui]
yolo = false
`
	out := RemoveMcpServerText([]byte(original), "imagine")
	doc := map[string]any{}
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten config does not parse: %v\n%s", err, out)
	}
	if _, ok := doc["mcp_servers"]; ok {
		t.Fatalf("commented mcp server was kept:\n%s", out)
	}
	if tableAt(doc, "cli")["installer"] != "local" || tableAt(doc, "ui")["yolo"] != false {
		t.Fatalf("unrelated sections changed:\n%s", out)
	}
}

func TestParseHeaderAllowsHashInsideQuotedKey(t *testing.T) {
	if got := parseHeader(`[ "a#b" ]`); got != `"a#b"` {
		t.Fatalf("quoted hash key = %q", got)
	}
	if got := parseHeader("[[model.foo]] # gone"); got != "model.foo" {
		t.Fatalf("array table = %q", got)
	}
	if got := parseHeader("# [models]"); got != "" {
		t.Fatalf("comment line = %q", got)
	}
}
