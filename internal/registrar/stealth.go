package registrar

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// seedAutomationProfile writes a lightweight Chromium profile that looks like a
// normal first-run user, without copying any real user data directory.
func seedAutomationProfile(profile string) error {
	defaultDir := filepath.Join(profile, "Default")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		return err
	}
	prefs := map[string]any{
		"credentials_enable_service": false,
		"profile": map[string]any{
			"password_manager_enabled": false,
			"exit_type":                "Normal",
			"exited_cleanly":           true,
			"content_settings": map[string]any{
				"exceptions": map[string]any{},
			},
			"default_content_setting_values": map[string]any{
				"notifications": 2,
				"geolocation":   2,
			},
		},
		"signin": map[string]any{
			"allowed": false,
		},
		"browser": map[string]any{
			"has_seen_welcome_page": true,
			"check_default_browser": false,
		},
		"distribution": map[string]any{
			"import_bookmarks":               false,
			"import_history":                 false,
			"import_search_engine":           false,
			"make_chrome_default":            false,
			"skip_first_run_ui":              true,
			"show_welcome_page":              false,
			"do_not_create_desktop_shortcut": true,
		},
	}
	raw, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(defaultDir, "Preferences"), raw, 0o600); err != nil {
		return err
	}
	localState := map[string]any{
		"browser": map[string]any{
			"enabled_labs_experiments": []string{},
		},
	}
	stateRaw, err := json.Marshal(localState)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(profile, "Local State"), stateRaw, 0o600)
}

// applyStealth patches automation fingerprints before page scripts run and
// sets a realistic viewport + Accept-Language. Works with temporary profiles.
func applyStealth(ctx context.Context, headless bool) error {
	width, height := 1366, 768
	scale := 1.0
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthInitScript).Do(ctx)
			return err
		}),
		emulation.SetDeviceMetricsOverride(int64(width), int64(height), scale, false),
		emulation.SetTouchEmulationEnabled(false),
		emulation.SetUserAgentOverride(chromeUserAgent).
			WithAcceptLanguage("en-US,en;q=0.9").
			WithPlatform("Win32"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			// Also patch the current about:blank document so the first navigate benefits.
			return chromedp.Evaluate(stealthInitScript, nil).Do(ctx)
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if headless {
				// Soften the most obvious headless UA token if Chromium injects one.
				return chromedp.Evaluate(`(() => {
try {
  const ua = navigator.userAgent.replace(/HeadlessChrome/gi, 'Chrome');
  Object.defineProperty(navigator, 'userAgent', { get: () => ua, configurable: true });
  Object.defineProperty(navigator, 'appVersion', { get: () => ua.replace(/^Mozilla\//, ''), configurable: true });
} catch (e) {}
})()`, nil).Do(ctx)
			}
			return nil
		}),
	)
}

// simulateHumanActivity performs short, low-amplitude pointer movement and
// scrolling. Cloudflare challenge scripts weigh trusted input events heavily.
func simulateHumanActivity(ctx context.Context) {
	x := 120 + randIntn(400)
	y := 140 + randIntn(280)
	_ = humanMoveTo(ctx, float64(x), float64(y))
	_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`window.scrollBy(0, %d)`, 20+randIntn(80)), nil))
	time.Sleep(time.Duration(80+randIntn(160)) * time.Millisecond)
	_ = humanMoveTo(ctx, float64(x+20+randIntn(80)), float64(y+10+randIntn(40)))
}

func humanMoveTo(ctx context.Context, x, y float64) error {
	steps := 6 + randIntn(8)
	startX := x - float64(40+randIntn(80))
	startY := y - float64(30+randIntn(60))
	if startX < 1 {
		startX = 1
	}
	if startY < 1 {
		startY = 1
	}
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		// Ease-in-out interpolation with tiny noise.
		ease := t * t * (3 - 2*t)
		cx := startX + (x-startX)*ease + float64(randIntn(3)-1)
		cy := startY + (y-startY)*ease + float64(randIntn(3)-1)
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchMouseEvent(input.MouseMoved, cx, cy).Do(ctx)
		})); err != nil {
			return err
		}
		time.Sleep(time.Duration(8+randIntn(18)) * time.Millisecond)
	}
	return nil
}

func humanClickXY(ctx context.Context, x, y float64) error {
	if err := humanMoveTo(ctx, x, y); err != nil {
		return err
	}
	time.Sleep(time.Duration(40+randIntn(90)) * time.Millisecond)
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MousePressed, x, y).
			WithButton(input.Left).
			WithClickCount(1).
			Do(ctx); err != nil {
			return err
		}
		time.Sleep(time.Duration(35+randIntn(55)) * time.Millisecond)
		return input.DispatchMouseEvent(input.MouseReleased, x, y).
			WithButton(input.Left).
			WithClickCount(1).
			Do(ctx)
	}))
}

type challengeWidget struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	Kind   string  `json:"kind"`
	OK     bool    `json:"ok"`
}

// locateChallengeWidget finds Turnstile / CF challenge widgets by geometry only
// (no cross-origin iframe DOM access required for the click target).
func locateChallengeWidget(ctx context.Context) (challengeWidget, error) {
	var widget challengeWidget
	err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
function visible(el) {
  if (!el) return false;
  const r = el.getBoundingClientRect();
  const s = getComputedStyle(el);
  return r.width > 8 && r.height > 8 && s.display !== 'none' && s.visibility !== 'hidden' && s.opacity !== '0';
}
function pack(el, kind) {
  const r = el.getBoundingClientRect();
  return { ok: true, x: r.left + r.width / 2, y: r.top + r.height / 2, width: r.width, height: r.height, kind };
}
const selectors = [
  'iframe[src*="challenges.cloudflare.com"]',
  'iframe[src*="turnstile"]',
  'iframe[title*="Widget containing a Cloudflare"]',
  'iframe[title*="cloudflare"]',
  'div.cf-turnstile',
  '[data-sitekey]',
  '#cf-turnstile',
  '.cf-challenge',
  'input[type="checkbox"][name*="cf"]',
];
for (const sel of selectors) {
  const nodes = document.querySelectorAll(sel);
  for (const n of nodes) {
    if (visible(n)) return pack(n, sel);
  }
}
// Managed interstitial often hosts a large challenge frame or center button-like node.
const text = (document.body && document.body.innerText || '').toLowerCase();
if (text.includes('verify you are human') || text.includes('just a moment') || text.includes('checking your browser')) {
  const candidates = Array.from(document.querySelectorAll('iframe, input[type="checkbox"], div[role="checkbox"], label, button'));
  for (const n of candidates) {
    if (!visible(n)) continue;
    const r = n.getBoundingClientRect();
    // Checkbox-sized or challenge iframe-sized hit targets.
    if ((r.width >= 20 && r.width <= 420 && r.height >= 20 && r.height <= 120) ||
        (r.width >= 200 && r.height >= 60)) {
      return pack(n, 'challenge-fallback');
    }
  }
}
return { ok: false, x: 0, y: 0, width: 0, height: 0, kind: '' };
})()`, &widget))
	return widget, err
}

// attemptTurnstileInteraction tries a trusted CDP click on the challenge widget
// and polls for a usable token / clearance signal.
func attemptTurnstileInteraction(ctx context.Context) (bool, error) {
	out := attemptTurnstileInteractionDetailed(ctx)
	return out.OK, out.Err
}

// attemptTurnstileInteractionDetailed is the same as attemptTurnstileInteraction
// but returns structured fields for status logging.
func attemptTurnstileInteractionDetailed(ctx context.Context) interactionOutcome {
	out := interactionOutcome{}
	// Prefer reading an already-issued token first.
	if n, _ := readAndInjectTurnstileToken(ctx); n >= 80 {
		out.OK = true
		out.Reason = "token_already_present"
		out.TokenLen = n
		out.PageState, _ = pageChallengeState(ctx)
		out.HasClearance, _ = hasCloudflareClearance(ctx)
		return out
	}
	widget, err := locateChallengeWidget(ctx)
	if err != nil {
		out.Err = err
		out.Reason = "locate_widget_error"
		return out
	}
	out.WidgetFound = widget.OK
	out.WidgetKind = widget.Kind
	if widget.OK {
		// Click slightly left of center — checkbox is usually on the left of the widget.
		clickX := widget.X
		clickY := widget.Y
		if widget.Width > 80 {
			clickX = widget.X - widget.Width*0.28 + float64(randIntn(6)-3)
		}
		if err := humanClickXY(ctx, clickX, clickY); err != nil {
			out.Err = err
			out.Reason = "click_error"
			return out
		}
		// Second micro-click near center helps some managed widgets.
		time.Sleep(time.Duration(250+randIntn(350)) * time.Millisecond)
		_ = humanClickXY(ctx, widget.X+float64(randIntn(5)-2), widget.Y+float64(randIntn(5)-2))
	} else {
		// Soft focus: click a neutral page point and let the extension try.
		_ = humanClickXY(ctx, float64(200+randIntn(100)), float64(240+randIntn(80)))
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
try { if (window.turnstile && typeof turnstile.execute === 'function') turnstile.execute(); } catch (e) {}
const iframes = document.querySelectorAll('iframe[src*="challenges.cloudflare.com"], iframe[src*="turnstile"]');
for (const iframe of iframes) {
  try { iframe.focus(); iframe.click(); } catch (e) {}
  try { iframe.contentWindow.postMessage({ event: 'challenge-complete' }, '*'); } catch (e) {}
}
return true;
})()`, nil))
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := readAndInjectTurnstileToken(ctx); n >= 80 {
			out.OK = true
			out.Reason = "token_after_click"
			out.TokenLen = n
			out.PageState, _ = pageChallengeState(ctx)
			out.HasClearance, _ = hasCloudflareClearance(ctx)
			return out
		}
		if ok, _ := hasCloudflareClearance(ctx); ok {
			out.OK = true
			out.Reason = "clearance_cookie"
			out.HasClearance = true
			out.TokenLen, _ = currentTurnstileTokenLen(ctx)
			out.PageState, _ = pageChallengeState(ctx)
			return out
		}
		if state, _ := pageChallengeState(ctx); state == "ready" {
			out.OK = true
			out.Reason = "page_ready"
			out.PageState = state
			out.TokenLen, _ = currentTurnstileTokenLen(ctx)
			out.HasClearance, _ = hasCloudflareClearance(ctx)
			return out
		}
		select {
		case <-ctx.Done():
			out.Err = ctx.Err()
			out.Reason = "context_done"
			out.PageState, _ = pageChallengeState(ctx)
			out.TokenLen, _ = currentTurnstileTokenLen(ctx)
			out.HasClearance, _ = hasCloudflareClearance(ctx)
			return out
		case <-time.After(400 * time.Millisecond):
		}
	}
	out.TokenLen, _ = currentTurnstileTokenLen(ctx)
	out.HasClearance, _ = hasCloudflareClearance(ctx)
	out.PageState, _ = pageChallengeState(ctx)
	if !out.WidgetFound {
		out.Reason = "no_widget_no_token"
	} else {
		out.Reason = "clicked_but_no_token"
	}
	return out
}

func currentTurnstileTokenLen(ctx context.Context) (int, error) {
	var n int
	err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
try {
  return String((document.querySelector('input[name="cf-turnstile-response"]') || {}).value || '').trim().length;
} catch (e) { return 0; }
})()`, &n))
	return n, err
}

func readAndInjectTurnstileToken(ctx context.Context) (int, error) {
	var token string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
try {
  const byInput = String((document.querySelector('input[name="cf-turnstile-response"]') || {}).value || '').trim();
  if (byInput.length >= 80) return byInput;
  if (window.turnstile && typeof turnstile.getResponse === 'function') {
    const r = String(turnstile.getResponse() || '').trim();
    if (r.length >= 80) return r;
  }
  // Some embeds stash the token on textarea siblings.
  const areas = document.querySelectorAll('textarea[name="cf-turnstile-response"], input[name*="turnstile"]');
  for (const a of areas) {
    const v = String(a.value || '').trim();
    if (v.length >= 80) return v;
  }
  return '';
} catch (e) { return ''; }
})()`, &token))
	token = strings.TrimSpace(token)
	if len(token) < 80 {
		return 0, nil
	}
	return injectTurnstileToken(ctx, token)
}

func hasCloudflareClearance(ctx context.Context) (bool, error) {
	var ok bool
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		cookies, err := network.GetCookies().Do(ctx)
		if err != nil {
			return err
		}
		for _, c := range cookies {
			name := strings.ToLower(c.Name)
			if name == "cf_clearance" && c.Value != "" {
				ok = true
				return nil
			}
		}
		return nil
	}))
	return ok, err
}

func pageChallengeState(ctx context.Context) (string, error) {
	var state string
	err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
const text=(document.body&&document.body.innerText||'').toLowerCase();
const title=(document.title||'').toLowerCase();
if(title.includes('just a moment')||text.includes('just a moment')||text.includes('checking your browser')||text.includes('verify you are human')||text.includes('needs to review the security'))return 'challenge';
if(title.includes('403')||text.includes('403 forbidden')||text.includes('access denied')||text.includes('sorry, you have been blocked'))return 'blocked';
const token=String((document.querySelector('input[name="cf-turnstile-response"]')||{}).value||'').trim();
if(token.length>=80)return 'ready';
const email=[...document.querySelectorAll('input[type="email"],input[name="email"],button,a')].some(n=>n&&n.getBoundingClientRect().width>0);
return email?'ready':'wait';
})()`, &state))
	return state, err
}

func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}

// stealthInitScript runs at document_start in every frame (via addScriptToEvaluateOnNewDocument).
const stealthInitScript = `(() => {
  if (window.__grokSwitchStealthApplied) return;
  window.__grokSwitchStealthApplied = true;

  const patch = (obj, prop, value) => {
    try {
      Object.defineProperty(obj, prop, {
        get: () => value,
        configurable: true,
        enumerable: true,
      });
    } catch (e) {}
  };

  try {
    patch(Navigator.prototype, 'webdriver', undefined);
    delete Navigator.prototype.webdriver;
  } catch (e) {}
  try {
    patch(navigator, 'webdriver', false);
  } catch (e) {}

  try {
    if (!window.chrome) window.chrome = {};
    if (!window.chrome.runtime) {
      window.chrome.runtime = {
        connect: function () {},
        sendMessage: function () {},
        id: undefined,
      };
    }
    if (!window.chrome.csi) window.chrome.csi = function () { return {}; };
    if (!window.chrome.loadTimes) window.chrome.loadTimes = function () { return {}; };
    if (!window.chrome.app) {
      window.chrome.app = {
        isInstalled: false,
        InstallState: { DISABLED: 'disabled', INSTALLED: 'installed', NOT_INSTALLED: 'not_installed' },
        RunningState: { CANNOT_RUN: 'cannot_run', READY_TO_RUN: 'ready_to_run', RUNNING: 'running' },
        getDetails: function () { return null; },
        getIsInstalled: function () { return false; },
      };
    }
  } catch (e) {}

  try {
    const originalQuery = window.navigator.permissions && window.navigator.permissions.query
      ? window.navigator.permissions.query.bind(window.navigator.permissions)
      : null;
    if (originalQuery) {
      window.navigator.permissions.query = (parameters) => {
        if (parameters && (parameters.name === 'notifications' || parameters.name === 'push')) {
          return Promise.resolve({ state: Notification.permission, onchange: null });
        }
        return originalQuery(parameters);
      };
    }
  } catch (e) {}

  try {
    patch(navigator, 'languages', Object.freeze(['en-US', 'en']));
    patch(navigator, 'language', 'en-US');
    patch(navigator, 'platform', 'Win32');
    patch(navigator, 'hardwareConcurrency', 8);
    patch(navigator, 'deviceMemory', 8);
    patch(navigator, 'maxTouchPoints', 0);
  } catch (e) {}

  try {
    const pluginData = [
      { name: 'PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Chrome PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Chromium PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'Microsoft Edge PDF Viewer', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
      { name: 'WebKit built-in PDF', filename: 'internal-pdf-viewer', description: 'Portable Document Format' },
    ];
    const plugins = pluginData.map((p) => {
      const plugin = Object.create(Plugin.prototype);
      Object.defineProperties(plugin, {
        name: { get: () => p.name },
        filename: { get: () => p.filename },
        description: { get: () => p.description },
        length: { get: () => 1 },
      });
      return plugin;
    });
    plugins.item = (i) => plugins[i] || null;
    plugins.namedItem = (n) => plugins.find((p) => p.name === n) || null;
    plugins.refresh = () => {};
    patch(navigator, 'plugins', plugins);
    patch(navigator, 'mimeTypes', {
      length: 2,
      item: () => null,
      namedItem: () => null,
      refresh: () => {},
    });
  } catch (e) {}

  try {
    const getParameter = WebGLRenderingContext.prototype.getParameter;
    WebGLRenderingContext.prototype.getParameter = function (parameter) {
      // UNMASKED_VENDOR_WEBGL / UNMASKED_RENDERER_WEBGL
      if (parameter === 37445) return 'Google Inc. (NVIDIA)';
      if (parameter === 37446) return 'ANGLE (NVIDIA, NVIDIA GeForce GTX 1660 SUPER Direct3D11 vs_5_0 ps_5_0, D3D11)';
      return getParameter.call(this, parameter);
    };
    if (typeof WebGL2RenderingContext !== 'undefined') {
      const getParameter2 = WebGL2RenderingContext.prototype.getParameter;
      WebGL2RenderingContext.prototype.getParameter = function (parameter) {
        if (parameter === 37445) return 'Google Inc. (NVIDIA)';
        if (parameter === 37446) return 'ANGLE (NVIDIA, NVIDIA GeForce GTX 1660 SUPER Direct3D11 vs_5_0 ps_5_0, D3D11)';
        return getParameter2.call(this, parameter);
      };
    }
  } catch (e) {}

  try {
    // iframe contentWindow.navigator.webdriver leak
    const descriptor = Object.getOwnPropertyDescriptor(HTMLIFrameElement.prototype, 'contentWindow');
    if (descriptor && descriptor.get) {
      const original = descriptor.get;
      Object.defineProperty(HTMLIFrameElement.prototype, 'contentWindow', {
        get: function () {
          const win = original.call(this);
          try {
            if (win) {
              Object.defineProperty(win.navigator, 'webdriver', { get: () => false, configurable: true });
            }
          } catch (e) {}
          return win;
        },
      });
    }
  } catch (e) {}

  try {
    Object.defineProperty(Notification, 'permission', { get: () => 'default', configurable: true });
  } catch (e) {}
})();
`
