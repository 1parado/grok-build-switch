package main

import (
	"bytes"
	"testing"

	"golang.org/x/net/html"
)

func TestNativeChatScrimSharesShellStackingContext(t *testing.T) {
	data, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	document, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	scrim := htmlElementByID(document, "nativeChatScrim")
	if scrim == nil {
		t.Fatal("nativeChatScrim not found")
	}
	if scrim.Parent == nil || !htmlElementHasClass(scrim.Parent, "nativeChatShell") {
		t.Fatal("nativeChatScrim must be a direct child of nativeChatShell so it stays below the mobile side panels")
	}
}

func TestCpaMintControlsHaveClientHandlers(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"startCpaMintBtn", "cancelCpaMintBtn", "openCpaMintUrlBtn", "grokPoolAuthDir"} {
		if !bytes.Contains(htmlData, []byte(`id="`+id+`"`)) {
			t.Fatalf("%s control not found", id)
		}
		if !bytes.Contains(appData, []byte(`$("`+id+`")`)) {
			t.Fatalf("%s client handler not found", id)
		}
	}
	for _, endpoint := range []string{"/api/cpa-mint", "/api/grok-pool/import-dir", "/api/grok-pool/open-auth-dir"} {
		if !bytes.Contains(appData, []byte(endpoint)) {
			t.Fatalf("client endpoint %s not found", endpoint)
		}
	}
}

func TestIndependentImageGenerationControlsHaveClientHandlers(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"imageGenEnabled", "imageGenFields", "imageGenBaseUrl", "imageGenApiKey",
		"imageGenApiBackend", "imageGenModel", "fetchImageModelsBtn", "testImageModelBtn",
	} {
		if !bytes.Contains(htmlData, []byte(`id="`+id+`"`)) {
			t.Fatalf("%s control not found", id)
		}
		if !bytes.Contains(appData, []byte(id)) {
			t.Fatalf("%s client handler not found", id)
		}
	}
	for _, removed := range []string{
		"featureImageGen", "featureImageEdit", "featureVideoGen",
		"featureImageGenModel", "featureImageEditModel", "featureVideoGenModel",
		"addImagineImageBtn", "addImagineImageQualityBtn", "addImagineVideoBtn",
	} {
		if bytes.Contains(htmlData, []byte(removed)) {
			t.Fatalf("removed media preset control %s is still present", removed)
		}
	}
	if !bytes.Contains(htmlData, []byte(`id="imageGenFields" class="imageGenFields" disabled`)) {
		t.Fatal("independent image fields should be disabled by default")
	}
	if !bytes.Contains(appData, []byte(`purpose: "image_generation"`)) {
		t.Fatal("image generation test must use the dedicated probe")
	}
}

func TestChatEmptyStateHasGuidance(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`id="chatEmpty"`,
		`id="chatEmptyTitle"`,
		`id="chatEmptyDesc"`,
		`id="chatEmptyStartBtn"`,
		`id="chatEmptyOpenContextBtn"`,
	} {
		if !bytes.Contains(htmlData, []byte(marker)) {
			t.Fatalf("chat empty marker %q not found in index.html", marker)
		}
	}
	for _, marker := range []string{
		"function renderChatEmptyState(",
		"function historyUserPresentation(",
		"sessionSwitchToken",
		`/api/agent/session/delete`,
		"cwd_missing",
		"/api/agent/upload",
		"/api/agent/rewind",
		"/api/agent/projects",
		"function rewindDropLastUser(",
		"function buildSlashCatalog(",
		"function showAgentPlan(",
		"function mountHistoryRange(",
		`mode: "prepend"`,
		"function setToolPayloadLazy(",
	} {
		if !bytes.Contains(appData, []byte(marker)) {
			t.Fatalf("chat fix marker %q not found in app.js", marker)
		}
	}
	// Context usage estimator was removed as inaccurate noise.
	if bytes.Contains(appData, []byte("function updateContextUsage(")) {
		t.Fatal("updateContextUsage should be removed from app.js")
	}
	if bytes.Contains(htmlData, []byte("contextUsageBar")) {
		t.Fatal("context usage UI should be removed from index.html")
	}
}

func TestMermaidIsLazyLoaded(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Eager <script src="...mermaid..."> would pull ~3.5MB on every page load.
	if bytes.Contains(htmlData, []byte(`src="/vendor/mermaid.min.js`)) {
		t.Fatal("index.html must not eagerly load mermaid; app.js loads it on demand")
	}
	for _, marker := range []string{
		`const MERMAID_SRC = "/vendor/mermaid.min.js`,
		"function loadMermaid(",
		"async function renderMermaidBlocks(",
	} {
		if !bytes.Contains(appData, []byte(marker)) {
			t.Fatalf("mermaid lazy-load marker %q not found in app.js", marker)
		}
	}
}

func TestChatRendersStructuredMediaEvents(t *testing.T) {
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	styleData, err := assets.ReadFile("ui/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`case "assistant_media":`,
		"/api/agent/media?session_id=",
		"localSessionMediaURL(",
		"function renderMessageMedia(",
		"function normalizeStructuredMedia(",
		"function extractMediaFromPayload(",
		"structuredMedia.length",
		"extractMediaFromPayload(tool.raw_output",
		"function isPlausibleMediaReference(",
		`document.createElement("video")`,
	} {
		if !bytes.Contains(appData, []byte(marker)) {
			t.Fatalf("structured chat media marker %q not found", marker)
		}
	}
	for _, marker := range []string{".chatMessageMedia", ".chatMediaItem video", ".chatMediaUnavailable"} {
		if !bytes.Contains(styleData, []byte(marker)) {
			t.Fatalf("structured chat media style %q not found", marker)
		}
	}
}

func TestRegistrarControlsHaveClientHandlers(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"registrarForm", "registrarSteps", "registrarAdvanced", "registrarCloudflareEssentials",
		"registrarProxyUrl", "registrarProxyStrategy", "registrarEngine", "registrarCloudflareApiBase",
		"probeRegistrarBtn", "startRegistrarBtn", "stopRegistrarBtn", "registrarLog",
		"registrarChallengeStatus", "registrarResults",
	} {
		if !bytes.Contains(htmlData, []byte(`id="`+id+`"`)) {
			t.Fatalf("%s control not found", id)
		}
	}
	for _, id := range []string{"registrarForm", "probeRegistrarBtn", "startRegistrarBtn", "stopRegistrarBtn", "registrarLog"} {
		if !bytes.Contains(appData, []byte(`$("`+id+`")`)) {
			t.Fatalf("%s client handler not found", id)
		}
	}
	if !bytes.Contains(appData, []byte("extractRegistrarChallengeStatus")) {
		t.Fatal("registrar challenge status renderer not found")
	}
	if !bytes.Contains(appData, []byte("renderRegistrarResults")) {
		t.Fatal("registrar per-account results renderer not found")
	}
	if !bytes.Contains(appData, []byte(`config.email_provider || "cloudflare"`)) {
		t.Fatal("registrar UI default email provider is not cloudflare")
	}
	if !bytes.Contains(htmlData, []byte("填写两项")) {
		t.Fatal("registrar 3-step guide not found")
	}
	for _, endpoint := range []string{"/api/registrar", "/api/registrar/probe", "/api/registrar/start", "/api/registrar/stop", "/api/registrar/job"} {
		if !bytes.Contains(appData, []byte(endpoint)) {
			t.Fatalf("client endpoint %s not found", endpoint)
		}
	}
	if !bytes.Contains(appData, []byte("registrarFormDirty")) {
		t.Fatal("registrar form dirty-state guard not found")
	}
}

func TestSkillsUIHasEmbeddedIconMarketplaceAndCollapsedGroups(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	iconData, err := assets.ReadFile("ui/skill.svg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(iconData, []byte("<svg")) {
		t.Fatal("embedded Skill SVG is invalid")
	}
	if !bytes.Contains(htmlData, []byte(`href="https://skillsmp.com/skills"`)) {
		t.Fatal("Skills marketplace link not found")
	}
	for _, marker := range []string{`class="skillsGroupHead"`, `aria-expanded="${expanded ? "true" : "false"}"`, `class="skillsItems"${expanded ? "" : " hidden"}`, `src="/skill.svg"`} {
		if !bytes.Contains(appData, []byte(marker)) {
			t.Fatalf("collapsible Skills UI marker %q not found", marker)
		}
	}
	styleData, err := assets.ReadFile("ui/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(styleData, []byte(".skillsItems[hidden] { display: none; }")) {
		t.Fatal("collapsed Skills items must be hidden despite flex display rules")
	}
}

func TestChatComposerUsesConfiguredModelsAndReasoningDefaults(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`id="composerModelSelect"`, `id="composerStrengthSelect"`, `composerSelectChip`, `composerFooterRow`} {
		if !bytes.Contains(htmlData, []byte(marker)) {
			t.Fatalf("composer control %q not found", marker)
		}
	}
	for _, marker := range []string{"loadComposerConfig", "default_reasoning_effort", "reasoning_efforts", "composerModelOverride", "composerStrengthOverride"} {
		if !bytes.Contains(appData, []byte(marker)) {
			t.Fatalf("configured composer marker %q not found", marker)
		}
	}
	if bytes.Contains(appData, []byte(`localStorage.getItem("gs_composer_model")`)) || bytes.Contains(appData, []byte(`localStorage.getItem("gs_composer_strength")`)) {
		t.Fatal("stale local composer settings must not override config defaults")
	}
}

func TestSelectedSkillPromptTextDoesNotRemainPopupFilter(t *testing.T) {
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	// After a completed `/skills name ` line, slash popup must close (null filter).
	for _, marker := range []string{
		`function slashPopupFilterQuery(`,
		`/^\/skills\s+(\S+)\s+/i`,
		`if (skillsMatch) return null;`,
		`function buildSlashCatalog(`,
	} {
		if !bytes.Contains(appData, []byte(marker)) {
			t.Fatalf("selected Skill query guard %q not found", marker)
		}
	}
}

func TestChatP0P1WorkbenchMarkers(t *testing.T) {
	htmlData, err := assets.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	appData, err := assets.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"agentProjectList", "addProjectBtn", "planBar", "permissionSessionBtn",
		"chatWorkspaceFileBtn", "agentSessionAutoApprove", "agentCwd",
		"openLocationBtn", "composerProjectBtn", "composerAccessSelect",
		"navHomeFromChatBtn", "agentSidebarTree", "agentSessionList",
		"toggleOrphanSessionsBtn",
	} {
		if !bytes.Contains(htmlData, []byte(`id="`+id+`"`)) {
			t.Fatalf("missing chat control %s", id)
		}
	}
	for _, marker := range []string{
		"function sessionsForProject(",
		"function orphanSessions(",
		"function orphanWorkspaceGroups(",
		"function renderSidebarTree(",
		"function activateProjectWorkspace(",
		"function promoteWorkspaceToProject(",
		"其他工作空间",
	} {
		if !bytes.Contains(appData, []byte(marker)) && !bytes.Contains(htmlData, []byte(marker)) {
			t.Fatalf("missing project-scoped session helper %q", marker)
		}
	}
	for _, endpoint := range []string{
		"/api/agent/upload", "/api/agent/rewind", "/api/agent/projects",
		"/api/agent/fs", "/api/agent/session/bootstrap",
		"/api/agent/pick-directory",
		"/api/agent/open-path",
		`type: "plan_response"`,
		"function pickWorkingDirectory(",
		"function pickDirectoryPath(",
		"function bindComposerFloatPad(",
		"function updateComposerProjectLabel(",
		"composerFooterRow",
	} {
		if !bytes.Contains(appData, []byte(endpoint)) && !bytes.Contains(htmlData, []byte(endpoint)) {
			// float pad / labels are JS; footer class is HTML
			if endpoint == "composerFooterRow" {
				if !bytes.Contains(htmlData, []byte(endpoint)) {
					t.Fatalf("missing client endpoint/marker %s", endpoint)
				}
				continue
			}
			if !bytes.Contains(appData, []byte(endpoint)) {
				t.Fatalf("missing client endpoint/marker %s", endpoint)
			}
		}
	}
}

func htmlElementByID(node *html.Node, id string) *html.Node {
	if node.Type == html.ElementNode {
		for _, attribute := range node.Attr {
			if attribute.Key == "id" && attribute.Val == id {
				return node
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := htmlElementByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func htmlElementHasClass(node *html.Node, className string) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	for _, attribute := range node.Attr {
		if attribute.Key != "class" {
			continue
		}
		for _, current := range bytes.Fields([]byte(attribute.Val)) {
			if string(current) == className {
				return true
			}
		}
	}
	return false
}
