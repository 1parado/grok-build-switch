package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTildeRuleMatchesHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatal(err)
	}
	engine, err := NewEngine(filepath.Join(t.TempDir(), "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AddUserRule("write(~/.ssh/**)", Deny); err != nil {
		t.Fatal(err)
	}
	engine.SetMode(ModeYolo)

	key := filepath.Join(home, ".ssh", "id_ed25519")
	if got := engine.Check("write", pathArg(key)); got != "deny" {
		t.Fatalf("write %s = %s, want deny", key, got)
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"~/.ssh/id_x"}`)); got != "deny" {
		t.Fatalf("literal tilde path = %s, want deny", got)
	}
	sibling := filepath.Join(home, ".ssh-other", "id_ed25519")
	if got := engine.Check("write", pathArg(sibling)); got != "allow" {
		t.Fatalf("write %s = %s, want allow", sibling, got)
	}
	// No session directory: a relative path stays literal and does not match.
	if got := engine.Check("write", json.RawMessage(`{"path":"../.ssh/id_ed25519"}`)); got != "allow" {
		t.Fatalf("relative path without a base = %s, want allow", got)
	}
	unclean := filepath.Join(home, "work") + string(filepath.Separator) + ".." + string(filepath.Separator) + ".ssh" + string(filepath.Separator) + "id_ed25519"
	if got := engine.Check("write", pathArg(unclean)); got != "deny" {
		t.Fatalf("write %s = %s, want deny", unclean, got)
	}

	engine.SetMode(ModeManual)
	if err := engine.AddUserRule("write(~/projects/**)", Allow); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, "projects", "a.go")
	if got := engine.Check("write", pathArg(project)); got != "allow" {
		t.Fatalf("write %s = %s, want allow", project, got)
	}
	other := filepath.Join(home, "projects-other", "a.go")
	if got := engine.Check("write", pathArg(other)); got != "ask" {
		t.Fatalf("write %s = %s, want ask", other, got)
	}
}

func TestRelativePathUsesSessionBase(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatal(err)
	}
	engine, err := NewEngine(filepath.Join(t.TempDir(), "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AddUserRule("write(~/.ssh/**)", Deny); err != nil {
		t.Fatal(err)
	}
	engine.SetMode(ModeYolo)
	engine.SetPathBase(filepath.Join(home, "work"))
	if got := engine.Check("write", json.RawMessage(`{"path":"../.ssh/id_ed25519"}`)); got != "deny" {
		t.Fatalf("relative path from the session = %s, want deny", got)
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"notes/a.go"}`)); got != "allow" {
		t.Fatalf("unrelated relative path = %s, want allow", got)
	}
}

func TestAbsoluteHomeRuleMatchesTildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatal(err)
	}
	engine, err := NewEngine(filepath.Join(t.TempDir(), "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	rule := "write(" + filepath.ToSlash(filepath.Join(home, ".ssh", "**")) + ")"
	if err := engine.AddUserRule(rule, Deny); err != nil {
		t.Fatal(err)
	}
	engine.SetMode(ModeYolo)
	if got := engine.Check("write", json.RawMessage(`{"path":"~/.ssh/id_x"}`)); got != "deny" {
		t.Fatalf("tilde path against %s = %s, want deny", rule, got)
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"~/projects/a.go"}`)); got != "allow" {
		t.Fatalf("unrelated tilde path = %s, want allow", got)
	}
}

func pathArg(path string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))
}
