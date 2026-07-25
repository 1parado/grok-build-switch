package registrar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// mintWithCurlCFFI runs scripts/sso_cpa_mint.py (curl_cffi Chrome TLS fingerprint).
// This is the most reliable pure-SSO → CPA path when Python + curl_cffi are available.
// Browser is NOT required; SPA false /done is avoided entirely.
// extraCookies may include cf_clearance / __cf_bm from the live Chrome session.
func mintWithCurlCFFI(ctx context.Context, sso, proxy string, extraCookies map[string]string, log func(string)) (mintTokens, error) {
	sso = strings.TrimSpace(sso)
	if sso == "" {
		return mintTokens{}, fmt.Errorf("empty sso")
	}
	script, err := resolveCFFIMintScript()
	if err != nil {
		return mintTokens{}, err
	}
	python, err := findPython()
	if err != nil {
		return mintTokens{}, err
	}

	payload, _ := json.Marshal(map[string]any{
		"sso":          sso,
		"proxy":        strings.TrimSpace(proxy),
		"timeout":      30,
		"poll_timeout": 90,
		"cookies":      extraCookies,
	})

	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(runCtx, python, script)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Hide console window on Windows when launched from GUI.
	hidePythonConsole(cmd)

	mintLog(log, "curl_cffi 协议铸造（Chrome TLS + SSO HTTP verify/approve）…")
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stdout.String())
		if msg == "" {
			msg = strings.TrimSpace(stderr.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		// Try parse error JSON from stdout even on non-zero exit.
		if tokens, perr := parseCFFIMintOutput(stdout.Bytes()); perr == nil {
			return tokens, nil
		}
		return mintTokens{}, fmt.Errorf("curl_cffi mint: %s", compactMintBody([]byte(msg)))
	}
	return parseCFFIMintOutput(stdout.Bytes())
}

func parseCFFIMintOutput(raw []byte) (mintTokens, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return mintTokens{}, fmt.Errorf("curl_cffi 无输出（请确认已 pip install curl_cffi）")
	}
	// Last non-empty line may be the JSON if there was logging noise.
	lines := bytes.Split(raw, []byte("\n"))
	var last []byte
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) > 0 && line[0] == '{' {
			last = line
			break
		}
	}
	if len(last) == 0 {
		last = raw
	}
	var resp struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(last, &resp); err != nil {
		return mintTokens{}, fmt.Errorf("解析 curl_cffi 输出: %w body=%s", err, compactMintBody(raw))
	}
	if !resp.OK {
		errMsg := strings.TrimSpace(resp.Error)
		if errMsg == "" {
			errMsg = "unknown error"
		}
		return mintTokens{}, fmt.Errorf("%s", errMsg)
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		return mintTokens{}, fmt.Errorf("curl_cffi 返回空 token")
	}
	return mintTokens{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		IDToken:      resp.IDToken,
		ExpiresIn:    resp.ExpiresIn,
	}, nil
}

func resolveCFFIMintScript() (string, error) {
	// 1) Next to executable
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "scripts", "sso_cpa_mint.py")
		if st, e := os.Stat(cand); e == nil && !st.IsDir() {
			return cand, nil
		}
	}
	// 2) Working directory / source tree
	for _, base := range []string{".", ".."} {
		cand := filepath.Join(base, "scripts", "sso_cpa_mint.py")
		if st, e := os.Stat(cand); e == nil && !st.IsDir() {
			abs, _ := filepath.Abs(cand)
			return abs, nil
		}
	}
	// 3) Relative to this source file at dev time
	_, file, _, ok := runtime.Caller(0)
	if ok {
		// internal/registrar → repo root
		root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		cand := filepath.Join(root, "scripts", "sso_cpa_mint.py")
		if st, e := os.Stat(cand); e == nil && !st.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("未找到 scripts/sso_cpa_mint.py")
}

// exportBrowserCookieMap pulls name→value for xAI cookies from Chrome (best-effort).
func exportBrowserCookieMap(browserCtx context.Context) map[string]string {
	out := map[string]string{}
	if browserCtx == nil || browserCtx.Err() != nil {
		return out
	}
	var cookies []*network.Cookie
	_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{
			"https://accounts.x.ai/",
			"https://auth.x.ai/",
			"https://grok.com/",
		}).Do(ctx)
		return err
	}))
	for _, c := range cookies {
		if c == nil || c.Name == "" || c.Value == "" {
			continue
		}
		// Prefer first seen; sso already handled separately.
		if _, ok := out[c.Name]; !ok {
			out[c.Name] = c.Value
		}
	}
	return out
}

func findPython() (string, error) {
	candidates := []string{"python", "python3", "py"}
	if runtime.GOOS == "windows" {
		candidates = []string{"python", "py", "python3"}
	}
	for _, name := range candidates {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		// `py` launcher needs `-3` sometimes; try plain first.
		cmd := exec.Command(path, "-c", "import curl_cffi; print('ok')")
		hidePythonConsole(cmd)
		out, err := cmd.CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("ok")) {
			return path, nil
		}
	}
	return "", fmt.Errorf("未找到带 curl_cffi 的 Python（请 pip install curl_cffi）")
}
