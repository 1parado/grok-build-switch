package registrar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedAutomationProfileWritesPrefs(t *testing.T) {
	dir := t.TempDir()
	if err := seedAutomationProfile(dir); err != nil {
		t.Fatal(err)
	}
	prefsPath := filepath.Join(dir, "Default", "Preferences")
	raw, err := os.ReadFile(prefsPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, needle := range []string{
		"password_manager_enabled",
		"has_seen_welcome_page",
		"skip_first_run_ui",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("preferences missing %q: %s", needle, text)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Local State")); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeTurnstileExtension(t *testing.T) {
	dir := t.TempDir()
	ext, err := materializeTurnstileExtension(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(ext, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "Turnstile Patch") {
		t.Fatalf("unexpected manifest: %s", manifest)
	}
	content, err := os.ReadFile(filepath.Join(ext, "content.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "autoClickTurnstile") {
		t.Fatalf("content.js missing autoClickTurnstile")
	}
}

func TestStealthInitScriptCoversWebdriver(t *testing.T) {
	for _, needle := range []string{
		"webdriver",
		"AutomationControlled",
		"WebGLRenderingContext",
		"hardwareConcurrency",
		"__grokSwitchStealthApplied",
	} {
		// AutomationControlled is a Chrome flag, not necessarily in the JS payload.
		if needle == "AutomationControlled" {
			continue
		}
		if !strings.Contains(stealthInitScript, needle) {
			t.Fatalf("stealth script missing %q", needle)
		}
	}
}

func TestRandIntnBounds(t *testing.T) {
	if randIntn(0) != 0 {
		t.Fatal("randIntn(0) should be 0")
	}
	for i := 0; i < 50; i++ {
		v := randIntn(7)
		if v < 0 || v >= 7 {
			t.Fatalf("randIntn(7)=%d out of range", v)
		}
	}
}
