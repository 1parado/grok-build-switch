package registrar

import (
	"context"
	"testing"
)

func TestClashRotatorDisabled(t *testing.T) {
	if r := NewClashRotator("", ""); r.Available() {
		t.Fatal("empty controller should be disabled")
	}
	if r := NewClashRotator("http://127.0.0.1:9090", ""); r.Available() {
		t.Fatal("empty group should be disabled")
	}
}

func TestClashRotatorNextNodeRoundRobin(t *testing.T) {
	r := &ClashRotator{
		controller: "http://127.0.0.1:9090",
		group:      "G",
		nodes:      []string{"A", "B", "C"},
	}
	// Two workers pick distinct nodes.
	n1 := r.NextNode()
	n2 := r.NextNode()
	if n1 != "A" || n2 != "B" {
		t.Fatalf("expected A then B, got %q then %q", n1, n2)
	}
	// Wrap around.
	r.NextNode() // C
	n4 := r.NextNode()
	if n4 != "A" {
		t.Fatalf("expected wrap to A, got %q", n4)
	}
}

func TestClashRotatorSelectNodeNoopWhenUnavailable(t *testing.T) {
	r := &ClashRotator{}
	// SelectNode on a disabled rotator is a no-op (no HTTP call).
	if err := r.SelectNode(context.Background(), "X"); err != nil {
		t.Fatalf("disabled SelectNode should be noop, got %v", err)
	}
}

func TestJSONStringEscapes(t *testing.T) {
	cases := map[string]string{
		`simple`:      `"simple"`,
		`has"quote`:   `"has\"quote"`,
		`back\slash`:  `"back\\slash"`,
		`🇭🇰 香港`:       `"🇭🇰 香港"`,
		"tab\there":   `"tab\there"`,
		"\x01control": `"\u0001control"`,
	}
	for in, want := range cases {
		if got := jsonString(in); got != want {
			t.Errorf("jsonString(%q) = %s, want %s", in, got, want)
		}
	}
}
