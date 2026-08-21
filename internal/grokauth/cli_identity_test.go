package grokauth

import (
	"net/http"
	"testing"
)

func TestResolveCLIVersionPreservesSupportedInbound(t *testing.T) {
	if got := ResolveCLIVersion("0.2.114"); got != "0.2.114" {
		t.Fatalf("ResolveCLIVersion(0.2.114) = %q", got)
	}
	if got := ResolveCLIVersion("0.2.93"); got != "0.2.93" {
		t.Fatalf("ResolveCLIVersion(0.2.93) = %q", got)
	}
	if got := ResolveCLIVersion(""); got != CLIClientVersion {
		t.Fatalf("ResolveCLIVersion(empty) = %q, want %q", got, CLIClientVersion)
	}
	if got := ResolveCLIVersion("0.2.1"); got != CLIClientVersion {
		t.Fatalf("ResolveCLIVersion(old) = %q, want %q", got, CLIClientVersion)
	}
}

func TestApplyCLIProxyHeadersFillsIdentityAndModelOverride(t *testing.T) {
	out := make(http.Header)
	ApplyCLIProxyHeaders(out, nil, "grok-4.6")
	if out.Get("X-XAI-Token-Auth") != CLITokenAuth {
		t.Fatalf("token auth = %q", out.Get("X-XAI-Token-Auth"))
	}
	if out.Get("x-grok-client-version") != CLIClientVersion {
		t.Fatalf("client version = %q", out.Get("x-grok-client-version"))
	}
	if out.Get("x-grok-client-identifier") != CLIClientIdentifier {
		t.Fatalf("identifier = %q", out.Get("x-grok-client-identifier"))
	}
	if out.Get("x-grok-model-override") != "grok-4.6" {
		t.Fatalf("model override = %q", out.Get("x-grok-model-override"))
	}
	if out.Get("User-Agent") != CLIUserAgent(CLIClientVersion) {
		t.Fatalf("user-agent = %q", out.Get("User-Agent"))
	}
}

func TestApplyCLIProxyHeadersPreservesInboundGrokHeaders(t *testing.T) {
	inbound := make(http.Header)
	inbound.Set("x-grok-client-version", "0.2.120")
	inbound.Set("x-grok-client-identifier", "grok-shell")
	inbound.Set("User-Agent", "xai-grok-workspace/0.2.120")
	inbound.Set("x-grok-model-override", "grok-build")
	inbound.Set("x-grok-conv-id", "conv-1")
	inbound.Set("x-grok-doom-loop-check", "1")
	out := make(http.Header)
	ApplyCLIProxyHeaders(out, inbound, "grok-4.6")
	if out.Get("x-grok-client-version") != "0.2.120" {
		t.Fatalf("version overwritten: %q", out.Get("x-grok-client-version"))
	}
	if out.Get("User-Agent") != "xai-grok-workspace/0.2.120" {
		t.Fatalf("ua overwritten: %q", out.Get("User-Agent"))
	}
	if out.Get("x-grok-model-override") != "grok-build" {
		t.Fatalf("override overwritten: %q", out.Get("x-grok-model-override"))
	}
	if out.Get("x-grok-conv-id") != "conv-1" || out.Get("x-grok-doom-loop-check") != "1" {
		t.Fatalf("passthrough missing: %#v", out)
	}
}
