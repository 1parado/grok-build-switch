package grokauth

import (
	"net/http"
	"strconv"
	"strings"
)

const (
	// CLIProxyHost is the hostname that requires official CLI identity headers.
	CLIProxyHost = "cli-chat-proxy.grok.com"

	// CLIStableVersion is the known-good minimum client version accepted by
	// cli-chat-proxy. Inbound versions below this are replaced.
	CLIStableVersion = "0.2.93"

	// CLIClientVersion is the pinned identity advertised when the inbound
	// request has no usable x-grok-client-version. Keep aligned with sub2api
	// / x.ai CLI chat-proxy.
	CLIClientVersion = "0.2.114"

	// CLITokenAuth is required by cli-chat-proxy for Grok Build OAuth tokens.
	CLITokenAuth = "xai-grok-cli"

	// CLIClientIdentifier is the x-grok-client-identifier used by Grok shell/CLI.
	CLIClientIdentifier = "grok-shell"

	// CLIClientMode is the interactive CLI surface expected by some probes.
	CLIClientMode = "interactive"
)

// ResolveCLIVersion returns a supported CLI client version. A valid inbound
// version at or above CLIStableVersion is preserved so a newer Grok CLI is
// not downgraded; anything else falls back to the pinned CLIClientVersion.
func ResolveCLIVersion(inbound string) string {
	inbound = strings.TrimSpace(inbound)
	if isSupportedCLIVersion(inbound) {
		return inbound
	}
	return CLIClientVersion
}

func isSupportedCLIVersion(version string) bool {
	major, minor, patch, ok := parseDottedVersion(version)
	if !ok {
		return false
	}
	minMajor, minMinor, minPatch, _ := parseDottedVersion(CLIStableVersion)
	if major != minMajor {
		return major > minMajor
	}
	if minor != minMinor {
		return minor > minMinor
	}
	return patch >= minPatch
}

func parseDottedVersion(version string) (int, int, int, bool) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	patch, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	if major < 0 || minor < 0 || patch < 0 {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// CLIUserAgent builds the workspace-style User-Agent for a CLI client version.
func CLIUserAgent(version string) string {
	if strings.TrimSpace(version) == "" {
		version = CLIClientVersion
	}
	return "xai-grok-workspace/" + version
}

func looksLikeGrokUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return false
	}
	return strings.Contains(ua, "xai-grok-workspace/") ||
		strings.Contains(ua, "grok-shell/") ||
		strings.Contains(ua, "grok-pager/")
}

var grokPassthroughHeaders = []string{
	"x-grok-conv-id",
	"x-grok-session-id",
	"x-grok-req-id",
	"x-grok-agent-id",
	"x-grok-turn-id",
	"x-grok-doom-loop-check",
	"openai-beta",
}

// ApplyCLIProxyHeaders stamps cli-chat-proxy identity onto an outbound request.
// Inbound Grok CLI headers are preserved when present and valid; missing
// routing/identity headers are filled so OpenAI-compatible clients still hit
// the correct upstream backend.
func ApplyCLIProxyHeaders(out http.Header, inbound http.Header, model string) {
	if out == nil {
		return
	}
	if inbound == nil {
		inbound = make(http.Header)
	}
	version := ResolveCLIVersion(inbound.Get("x-grok-client-version"))
	out.Set("X-XAI-Token-Auth", CLITokenAuth)
	out.Set("x-grok-client-version", version)
	out.Set("X-Grok-Client-Version", version)

	if identifier := strings.TrimSpace(inbound.Get("x-grok-client-identifier")); identifier != "" {
		out.Set("x-grok-client-identifier", identifier)
	} else {
		out.Set("x-grok-client-identifier", CLIClientIdentifier)
	}

	if mode := strings.TrimSpace(firstNonEmpty(inbound.Get("x-grok-client-mode"), inbound.Get("X-Grok-Client-Mode"))); mode != "" {
		out.Set("x-grok-client-mode", mode)
	} else {
		out.Set("x-grok-client-mode", CLIClientMode)
		out.Set("X-Grok-Client-Mode", CLIClientMode)
	}

	if ua := strings.TrimSpace(inbound.Get("User-Agent")); looksLikeGrokUserAgent(ua) {
		out.Set("User-Agent", ua)
	} else {
		out.Set("User-Agent", CLIUserAgent(version))
	}

	override := strings.TrimSpace(inbound.Get("x-grok-model-override"))
	if override == "" {
		override = strings.TrimSpace(model)
	}
	if override != "" {
		out.Set("x-grok-model-override", override)
	}

	for _, key := range grokPassthroughHeaders {
		if value := strings.TrimSpace(inbound.Get(key)); value != "" && out.Get(key) == "" {
			out.Set(key, value)
		}
	}
	out.Del("x-api-key")
}
