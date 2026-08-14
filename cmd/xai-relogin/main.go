// xai-relogin — 批量刷新 x.ai/Grok 账号凭据（两段式）。
//
// 两段式刷新：优先用 cpa_auths 里现有凭据文件的 refresh_token 直接续期
// （秒级、不弹浏览器）；续期失败（吊销/过期/网络）才回退为浏览器重新登录并
// 重新铸造 OAuth token。
//
// 用法：
//
//	go run ./cmd/xai-relogin -limit 3
//	go run ./cmd/xai-relogin -emails tmp39i8e4b8n3y@paradox29.xyz
//	go run ./cmd/xai-relogin -pool
//	go run ./cmd/xai-relogin -proxy http://127.0.0.1:7897
//
// 默认：
//   - 目标账号：%USERPROFILE%\.grok_switch\cpa_auths 下全部 xai-*.json（-pool 时只保留号池里的）
//   - 密码：%USERPROFILE%\.grok_switch\registrar\accounts_cli.txt（邮箱----密码----jwt）
//   - 配置：%USERPROFILE%\.grok_switch\registrar.json
//   - 输出：新 CPA 写回 cpa_auths，cookie 快照写 registrar\cookies
//   - 报告：%USERPROFILE%\.grok_switch\registrar\relogin-report-<时间戳>.json
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"grok_switch/internal/registrar"
)

func main() {
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".grok_switch")

	var (
		configPath   string
		accountsPath string
		authDir      string
		cookieDir    string
		reportPath   string
		poolPath     string
		emailsFlag   string
		proxy        string
		browserMode  string
		poolOnly     bool
		limit        int
	)
	flag.StringVar(&configPath, "config", filepath.Join(dataDir, "registrar.json"), "registrar 配置 JSON 路径")
	flag.StringVar(&accountsPath, "accounts", filepath.Join(dataDir, "registrar", "accounts_cli.txt"), "密码账本（邮箱----密码----jwt 每行一个）")
	flag.StringVar(&authDir, "auth-dir", filepath.Join(dataDir, "cpa_auths"), "CPA 凭据目录（默认扫描全部 xai-*.json 作为目标）")
	flag.StringVar(&cookieDir, "cookie-dir", filepath.Join(dataDir, "registrar", "cookies"), "cookie 快照输出目录")
	flag.StringVar(&reportPath, "report", "", "结果报告输出路径（默认 registrar\\relogin-report-<时间戳>.json）")
	flag.StringVar(&poolPath, "pool-path", filepath.Join(dataDir, "grok_pool", "pool.json"), "号池 pool.json 路径")
	flag.StringVar(&emailsFlag, "emails", "", "只处理这些邮箱（逗号分隔）")
	flag.StringVar(&proxy, "proxy", "", "覆盖代理地址（留空用配置里的）")
	flag.StringVar(&browserMode, "browser-mode", "", "覆盖浏览器模式（visible/headless，留空用配置里的）")
	flag.BoolVar(&poolOnly, "pool", false, "只处理号池 pool.json 里的账号（默认处理 auth-dir 下全部 xai-*.json）")
	flag.IntVar(&limit, "limit", 0, "最多处理 N 个账号；0=全部")
	flag.Parse()

	// 1) 配置。
	cfg := registrar.DefaultConfig()
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "警告：解析 %s 失败（%v），使用默认配置\n", configPath, err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "警告：读取 %s 失败（%v），使用默认配置（无代理）\n", configPath, err)
	}
	if proxy != "" {
		cfg.ProxyURL = proxy
	}
	if browserMode != "" {
		cfg.BrowserMode = browserMode
	}
	if cfg.PageTimeoutSeconds <= 0 {
		cfg.PageTimeoutSeconds = 300
	}
	if cfg.Count <= 0 {
		cfg.Count = 1
	}

	// 2) 目标账号：扫描 auth-dir 下全部 xai-*.json 凭据文件。
	emails, err := scanAuthFiles(authDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "扫描凭据目录失败:", err)
		os.Exit(1)
	}
	if len(emails) == 0 {
		fmt.Fprintln(os.Stderr, "没有发现凭据文件（检查 -auth-dir）")
		os.Exit(1)
	}
	if poolOnly {
		poolEmails, perr := poolAccountEmails(poolPath)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "读取号池失败，忽略 -pool:", perr)
		} else {
			keep := map[string]bool{}
			for _, e := range poolEmails {
				if e = strings.TrimSpace(e); e != "" {
					keep[strings.ToLower(e)] = true
				}
			}
			filtered := emails[:0]
			for _, e := range emails {
				if keep[strings.ToLower(e)] {
					filtered = append(filtered, e)
				}
			}
			emails = filtered
		}
	}
	only := map[string]bool{}
	for _, e := range strings.Split(emailsFlag, ",") {
		if e = strings.TrimSpace(e); e != "" {
			only[strings.ToLower(e)] = true
		}
	}
	passwords := ledgerPasswords(accountsPath)
	var targets []account
	for _, e := range emails {
		if len(only) > 0 && !only[strings.ToLower(e)] {
			continue
		}
		pw, ok := passwords[strings.ToLower(e)]
		targets = append(targets, account{email: e, password: pw, hasPassword: ok})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].email < targets[j].email })
	if limit > 0 && len(targets) > limit {
		targets = targets[:limit]
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "没有待处理的账号（检查 -auth-dir / -pool / -emails / -limit）")
		os.Exit(1)
	}

	// 3) 目录就绪。
	for _, d := range []string{authDir, cookieDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "创建目录失败:", d, err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 4) 两段式逐个刷新。
	log := func(msg string) {
		fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), msg)
	}
	withPassword := 0
	for _, t := range targets {
		if t.hasPassword {
			withPassword++
		}
	}
	log(fmt.Sprintf("开始批量刷新：%d 个账号（其中 %d 个有密码），代理=%s，浏览器模式=%s",
		len(targets), withPassword, redactOrEmpty(cfg.ProxyURL), cfg.BrowserMode))

	results := make([]registrar.ReloginResult, 0, len(targets))
	for i, acc := range targets {
		log(fmt.Sprintf("==== [%d/%d] %s ====", i+1, len(targets), acc.email))
		if !acc.hasPassword {
			log(fmt.Sprintf("⏭️  %s 跳过：accounts_cli.txt 账本没有该邮箱密码", acc.email))
			results = append(results, registrar.ReloginResult{
				Email:  acc.email,
				Status: "failed",
				Error:  "accounts_cli.txt 账本没有该邮箱密码",
			})
			continue
		}
		res, err := registrar.RefreshAccount(ctx, cfg, acc.email, acc.password, authDir, cookieDir, log)
		if err != nil && res.Status == "" {
			res.Status = "failed"
			res.Error = err.Error()
		}
		results = append(results, res)
		if res.Status == "success" {
			if res.MintMethod == "refresh" {
				log(fmt.Sprintf("✅ %s 续期成功（refresh_token 直续，无浏览器）", acc.email))
			} else {
				log(fmt.Sprintf("✅ %s 重新登录+铸造成功（%s）", acc.email, res.MintMethod))
			}
		} else {
			log(fmt.Sprintf("❌ %s 失败：%s", acc.email, res.Error))
		}
		if ctx.Err() != nil {
			log("收到中断信号，停止后续账号")
			break
		}
	}

	// 5) 报告。
	if reportPath == "" {
		reportPath = filepath.Join(dataDir, "registrar",
			fmt.Sprintf("relogin-report-%s.json", time.Now().Format("20060102-150405")))
	}
	summary := struct {
		GeneratedAt time.Time                 `json:"generated_at"`
		Total       int                       `json:"total"`
		Success     int                       `json:"success"`
		Failed      int                       `json:"failed"`
		ViaRefresh  int                       `json:"via_refresh"`
		ViaRelogin  int                       `json:"via_relogin"`
		Results     []registrar.ReloginResult `json:"results"`
	}{
		GeneratedAt: time.Now(),
		Total:       len(results),
		Results:     results,
	}
	for _, r := range results {
		if r.Status == "success" {
			summary.Success++
			if r.MintMethod == "refresh" {
				summary.ViaRefresh++
			} else {
				summary.ViaRelogin++
			}
		} else {
			summary.Failed++
		}
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(reportPath, append(data, '\n'), 0o600)

	fmt.Printf("\n完成：共 %d，成功 %d（直续 %d / 浏览器 %d），失败 %d\n报告：%s\n",
		summary.Total, summary.Success, summary.ViaRefresh, summary.ViaRelogin, summary.Failed, reportPath)
}

type account struct {
	email       string
	password    string
	hasPassword bool
}

// scanAuthFiles 列出 authDir 下所有 xai-<email>.json 并提取邮箱。
func scanAuthFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "xai-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		email := strings.TrimSuffix(strings.TrimPrefix(name, "xai-"), ".json")
		if strings.Contains(email, "@") {
			out = append(out, email)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ledgerPasswords 读取密码账本（邮箱----密码----jwt），返回 邮箱(小写)->密码。
func ledgerPasswords(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.Split(strings.TrimSpace(sc.Text()), "----")
		if len(parts) >= 2 && strings.Contains(parts[0], "@") {
			if pw := strings.TrimSpace(parts[1]); pw != "" {
				out[strings.ToLower(strings.TrimSpace(parts[0]))] = pw
			}
		}
	}
	return out
}

// poolAccountEmails 读取号池 pool.json 的账号邮箱列表。
func poolAccountEmails(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pool struct {
		Accounts []struct {
			Email string `json:"email"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range pool.Accounts {
		if email := strings.TrimSpace(a.Email); strings.Contains(email, "@") {
			out = append(out, email)
		}
	}
	return out, nil
}

func redactOrEmpty(proxy string) string {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return "直连"
	}
	if i := strings.LastIndex(proxy, "@"); i >= 0 {
		return proxy[:strings.Index(proxy, "://")+3] + "***" + proxy[i:]
	}
	return proxy
}
