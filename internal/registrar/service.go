package registrar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxLogTail = 240

type registrationOutcome struct {
	Email      string
	Password   string
	SSO        string
	MintMethod string
	AuthFile   string
	CookieFile string
}

type Service struct {
	mu           sync.Mutex
	ledgerMu     sync.Mutex
	dataDir      string
	configPath   string
	runtimeDir   string
	accountsPath string
	attemptsPath string
	config       Config
	current      *liveJob
	last         *Job
	authDir      func() string
	onFinished   func(Job)
	cookieDir    string
	runAccount   func(context.Context, Config, Mailbox, string, string, func(string)) (registrationOutcome, error)
}

type liveJob struct {
	job    Job
	cancel context.CancelFunc
	log    *os.File
}

// warmSlot holds a pre-started Chrome session so the next account can skip the
// 2-3s browser cold-start. Only used in single-worker serial mode.
type warmSlot struct {
	mu       sync.Mutex
	session  *browserSession
	headless bool
	starting bool
}

// take retrieves the warmed session, waiting briefly (up to 3s) if a warm
// launch is in progress. Returns nil if none is available within the window.
func (w *warmSlot) take() *browserSession {
	if w == nil {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		w.mu.Lock()
		if w.session != nil {
			s := w.session
			w.session = nil
			w.mu.Unlock()
			return s
		}
		starting := w.starting
		w.mu.Unlock()
		if !starting || time.Now().After(deadline) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// start launches a Chrome session in the background and stores it for the next
// account. If a warm session already exists (unused from a previous round), it
// is discarded first. Failures are silent — the next account cold-starts.
func (w *warmSlot) start(ctx context.Context, config Config, log func(string, string), jobID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.starting {
		w.mu.Unlock()
		return
	}
	// Discard any previous unused warm session.
	if w.session != nil {
		w.session.Close()
		w.session = nil
	}
	w.starting = true
	w.mu.Unlock()

	go func() {
		// Use a background-derived context so the warm browser survives the brief
		// gap between accounts. The job context is watched for cancellation.
		warmCtx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-warmCtx.Done():
			}
		}()
		warmConfig := config
		warmConfig.warmSession = nil // never carry a warm session into startBrowser
		session, err := startBrowser(warmCtx, warmConfig, w.headless)
		if err != nil {
			cancel()
			log(jobID, "预热浏览器启动失败（下一个账号将冷启动）："+err.Error())
			w.mu.Lock()
			w.starting = false
			w.mu.Unlock()
			return
		}
		w.mu.Lock()
		w.session = session
		w.starting = false
		w.mu.Unlock()
	}()
}

// discard closes any warmed session (called at job end / cancellation).
func (w *warmSlot) discard() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.session != nil {
		w.session.Close()
		w.session = nil
	}
}

func NewService(dataDir string) (*Service, error) {
	runtimeDir := filepath.Join(dataDir, "registrar")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "jobs"), 0o700); err != nil {
		return nil, err
	}
	s := &Service{
		dataDir:      dataDir,
		configPath:   filepath.Join(dataDir, "registrar.json"),
		runtimeDir:   runtimeDir,
		accountsPath: filepath.Join(runtimeDir, "accounts_cli.txt"),
		attemptsPath: filepath.Join(runtimeDir, "emails_attempted.txt"),
		cookieDir:    filepath.Join(runtimeDir, "cookies"),
		runAccount:   registerAccount,
	}
	if err := s.loadConfig(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) SetAuthDirResolver(fn func() string) {
	s.mu.Lock()
	s.authDir = fn
	s.mu.Unlock()
}

func (s *Service) SetOnFinished(fn func(Job)) {
	s.mu.Lock()
	s.onFinished = fn
	s.mu.Unlock()
}

func (s *Service) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Service) Update(config Config) (State, error) {
	config = normalizeConfig(config)
	if err := validateConfig(config, false); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		return State{}, fmt.Errorf("注册任务运行中，不能修改配置")
	}
	s.config = config
	if err := s.saveConfigLocked(); err != nil {
		return State{}, err
	}
	return s.stateLocked(), nil
}

func (s *Service) Start() (Job, error) {
	s.mu.Lock()
	if s.current != nil {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("已有注册任务正在运行")
	}
	config := normalizeConfig(s.config)
	if err := validateConfig(config, true); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	probe := s.probeConfigLocked(config)
	if !probe.OK {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("环境检测未通过: %s", failedChecks(probe))
	}
	// Mail provider uses the primary proxy (or direct) for mailbox HTTP APIs.
	mailConfig := config
	if pool := NewProxyPool(config); pool.Len() > 0 {
		mailConfig.ProxyURL = pool.Primary()
	} else {
		mailConfig.ProxyURL = ""
	}
	provider, err := newMailProvider(mailConfig, s.usedEmails())
	if err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	id, err := newJobID()
	if err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	logPath := filepath.Join(s.runtimeDir, "jobs", id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	live := &liveJob{
		job: Job{
			ID:        id,
			Status:    StatusStarting,
			Requested: config.Count,
			LogTail:   []string{},
			Results:   []AccountResult{},
			StartedAt: now,
		},
		cancel: cancel,
		log:    logFile,
	}
	s.current = live
	s.config = config
	s.config.LastJobID = id
	if err := s.saveConfigLocked(); err != nil {
		s.current = nil
		cancel()
		logFile.Close()
		s.mu.Unlock()
		return Job{}, err
	}
	live.job.Status = StatusRunning
	job := cloneJob(live.job)
	s.mu.Unlock()

	s.log(id, fmt.Sprintf("启动内置注册任务：数量=%d，并发=%d，邮箱=%s，引擎=%s",
		config.Count, config.Workers, config.EmailProvider, config.RegisterEngine))
	go s.run(ctx, live, config, provider)
	return job, nil
}

func (s *Service) Stop() (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return Job{}, fmt.Errorf("没有正在运行的注册任务")
	}
	s.current.cancel()
	return cloneJob(s.current.job), nil
}

func (s *Service) Close() {
	s.mu.Lock()
	if s.current != nil {
		s.current.cancel()
	}
	s.mu.Unlock()
}

func (s *Service) Job() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		job := cloneJob(s.current.job)
		return &job
	}
	if s.last == nil {
		return nil
	}
	job := cloneJob(*s.last)
	return &job
}

func (s *Service) ReadLog() ([]byte, error) {
	s.mu.Lock()
	id := ""
	if s.current != nil {
		id = s.current.job.ID
	} else if s.last != nil {
		id = s.last.ID
	}
	s.mu.Unlock()
	if id == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(s.runtimeDir, "jobs", id+".log"))
}

func (s *Service) SetImportResult(id string, imported, updated int, importErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var job *Job
	if s.current != nil && s.current.job.ID == id {
		job = &s.current.job
	} else if s.last != nil && s.last.ID == id {
		job = s.last
	}
	if job == nil {
		return
	}
	job.Imported = imported
	job.Updated = updated
	if importErr != nil {
		job.Error = strings.TrimSpace(job.Error + "; 号池导入: " + importErr.Error())
	}
}

func (s *Service) run(ctx context.Context, live *liveJob, config Config, provider MailProvider) {
	pool := NewProxyPool(config)
	gate := newBrowserGate(config, pool)
	if pool.Len() > 0 {
		s.log(live.job.ID, fmt.Sprintf("代理池：%d 条，策略=%s，冷却=%ds，引擎=%s",
			pool.Len(), config.ProxyStrategy, config.ProxyCooldownSeconds, config.RegisterEngine))
	} else {
		s.log(live.job.ID, fmt.Sprintf("代理：直连，引擎=%s", config.RegisterEngine))
	}
	s.log(live.job.ID, gate.Describe()+"（单代理/直连时强制串行，避免 ERR_CONNECTION_CLOSED）")
	if config.Workers > gate.Limit() {
		reason := "单代理/直连时强制串行，避免多开 Chrome 打爆本地 Clash（EOF / ERR_CONNECTION_CLOSED）"
		if pool != nil && pool.Len() > 1 {
			reason = "代理条数限制了同时浏览器数"
		}
		s.log(live.job.ID, fmt.Sprintf(
			"配置并发=%d，实际浏览器同时打开≤%d（%s；要真·多线程请配置 ≥%d 条不同代理）",
			config.Workers, gate.Limit(), reason, config.Workers,
		))
	}

	// Browser pre-warm: only useful in single-worker serial mode. When enabled,
	// each account launches Chrome for the NEXT account while the current one
	// is still running (during CPA mint), so the next account skips the 2-3s
	// cold-start and navigates immediately after acquiring the gate.
	workers := config.Workers
	if workers > config.Count {
		workers = config.Count
	}
	headless := browserHeadless(config)
	warmEnabled := workers == 1 && !headless && normalizeRegisterEngine(config.RegisterEngine) != "protocol_only"
	var warm *warmSlot
	if warmEnabled {
		warm = &warmSlot{headless: headless}
		s.log(live.job.ID, "浏览器预热已启用（串行模式下为下一个账号提前启动 Chrome）")
		// Kick off the first warm immediately so account #1 can also benefit.
		warm.start(ctx, config, s.log, live.job.ID)
	}

	tasks := make(chan int)
	var wg sync.WaitGroup
	for worker := 1; worker <= workers; worker++ {
		workerID := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range tasks {
				if ctx.Err() != nil {
					return
				}
				s.runOne(ctx, live.job.ID, config, provider, pool, gate, warm, workerID, index)
			}
		}()
	}
	for index := 1; index <= config.Count; index++ {
		select {
		case tasks <- index:
		case <-ctx.Done():
			close(tasks)
			wg.Wait()
			if warm != nil {
				warm.discard()
			}
			s.syncProviderCredentials(provider)
			s.finish(live, ctx.Err())
			return
		}
	}
	close(tasks)
	wg.Wait()
	if warm != nil {
		warm.discard()
	}
	s.syncProviderCredentials(provider)
	s.finish(live, ctx.Err())
}

func (s *Service) runOne(ctx context.Context, jobID string, config Config, provider MailProvider, pool *ProxyPool, gate *browserGate, warm *warmSlot, worker, index int) {
	mailbox, err := provider.Allocate(ctx)
	if err != nil {
		s.completeAccount(jobID, AccountResult{Status: "failed", Error: err.Error()})
		return
	}
	email := mailbox.Address()
	if err := s.appendAttempt(email); err != nil {
		s.completeAccount(jobID, AccountResult{Email: email, Status: "failed", Error: err.Error()})
		return
	}
	log := func(message string) {
		s.log(jobID, fmt.Sprintf("[W%d %d/%d %s] %s", worker, index, config.Count, email, message))
	}

	// Per-account proxy from the pool (sticky by index when strategy=sticky).
	accountConfig := config
	proxy := ""
	if pool != nil {
		proxy = pool.Pick(index - 1)
	}
	accountConfig.ProxyURL = proxy

	// Pick up a pre-warmed browser session (if the service warmed one for us).
	if warm != nil {
		if ws := warm.take(); ws != nil {
			if browserAlive(ws) {
				accountConfig.warmSession = ws
				log("使用预热浏览器（跳过冷启动）")
			} else {
				ws.Close()
			}
		}
	}

	// Serialize browser sessions (especially when all share one local proxy).
	log("等待浏览器名额…")
	if err := gate.Acquire(ctx); err != nil {
		if accountConfig.warmSession != nil {
			accountConfig.warmSession.Close()
		}
		s.completeAccount(jobID, AccountResult{Email: email, Status: "failed", Error: "任务已取消"})
		return
	}

	if proxy != "" {
		log("开始注册 · 代理 " + RedactProxy(proxy))
	} else {
		log("开始注册 · 直连")
	}

	outcome, err := s.runAccount(ctx, accountConfig, mailbox, s.resolvedAuthDir(), s.cookieDir, log)

	// Start warming a browser for the NEXT account BEFORE releasing the gate.
	// This gives the warm Chrome ~2-3s head start while the current browser
	// (inside runAccount) is being torn down via session.Close().
	if warm != nil {
		warm.start(ctx, accountConfig, s.log, jobID)
	}

	// Release the gate so the next account can acquire it immediately.
	gate.Release()


	if err != nil {
		if pool != nil && proxy != "" && shouldCoolProxy(proxy, pool, err) {
			pool.ReportFailure(proxy)
			log("代理标记冷却：" + RedactProxy(proxy))
		} else if proxy != "" && isTransientNavError(err) {
			log("网络瞬时失败，未冷却代理（单代理/本地代理）")
		}
		failMsg := err.Error()
		if isTransientNavError(err) {
			failMsg = failMsg + " | " + navigateHint(err, proxy, gate.Describe())
		}
		log("失败：" + failMsg)
		s.completeAccount(jobID, AccountResult{Email: email, Status: "failed", Error: failMsg})
		return
	}
	if pool != nil && proxy != "" {
		pool.ReportSuccess(proxy)
	}
	if err := s.appendAccount(outcome); err != nil {
		failMsg := "账号已注册，但账本写入失败：" + err.Error()
		log(failMsg)
		s.completeAccount(jobID, AccountResult{Email: email, Status: "failed", Error: failMsg, AuthFile: outcome.AuthFile, CookieFile: outcome.CookieFile})
		return
	}
	log("注册与 CPA 铸造成功")
	s.completeAccount(jobID, AccountResult{
		Email:      outcome.Email,
		Status:     "success",
		MintMethod: outcome.MintMethod,
		AuthFile:   outcome.AuthFile,
		CookieFile: outcome.CookieFile,
	})
}

func (s *Service) completeAccount(jobID string, result AccountResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.job.ID != jobID {
		return
	}
	job := &s.current.job
	job.Completed++
	if result.Status == "success" {
		job.Succeeded++
	} else {
		job.Failed++
	}
	job.Results = append(job.Results, result)
}

func (s *Service) finish(live *liveJob, runErr error) {
	s.mu.Lock()
	if s.current != live {
		s.mu.Unlock()
		return
	}
	live.job.FinishedAt = time.Now().UTC()
	if errors.Is(runErr, context.Canceled) {
		live.job.Status = StatusCancelled
		live.job.Error = "任务已取消"
		if summary := aggregateJobFailures(live.job.Results); strings.Contains(summary, "：") {
			live.job.Error = "任务已取消；" + strings.TrimPrefix(summary, "没有账号注册成功：")
		}
	} else if live.job.Succeeded > 0 {
		live.job.Status = StatusSucceeded
		if live.job.Failed > 0 {
			live.job.Error = fmt.Sprintf("部分成功：成功 %d，失败 %d。%s",
				live.job.Succeeded, live.job.Failed, strings.TrimPrefix(aggregateJobFailures(live.job.Results), "没有账号注册成功："))
		}
	} else {
		live.job.Status = StatusFailed
		live.job.Error = aggregateJobFailures(live.job.Results)
	}
	_ = live.log.Close()
	job := cloneJob(live.job)
	s.last = &job
	s.current = nil
	callback := s.onFinished
	s.mu.Unlock()
	if callback != nil {
		callback(job)
	}
}

func (s *Service) appendAccount(outcome registrationOutcome) error {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.accountsPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(s.accountsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s----%s----%s\n", outcome.Email, outcome.Password, outcome.SSO)
	return err
}

func (s *Service) appendAttempt(email string) error {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	file, err := os.OpenFile(s.attemptsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintln(file, strings.TrimSpace(email))
	return err
}

func (s *Service) usedEmails() map[string]bool {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	data, _ := os.ReadFile(s.accountsPath)
	used := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(line, "----")
		if len(parts) > 0 && strings.Contains(parts[0], "@") {
			used[strings.ToLower(strings.TrimSpace(parts[0]))] = true
		}
	}
	attempted, _ := os.ReadFile(s.attemptsPath)
	for _, line := range strings.Split(string(attempted), "\n") {
		if email := strings.ToLower(strings.TrimSpace(line)); strings.Contains(email, "@") {
			used[email] = true
		}
	}
	return used
}

func (s *Service) syncProviderCredentials(provider MailProvider) {
	if snapshotter, ok := provider.(credentialSnapshotter); ok {
		s.mu.Lock()
		s.config.HotmailAccountsText = snapshotter.CredentialsText()
		_ = s.saveConfigLocked()
		s.mu.Unlock()
	}
}

func (s *Service) log(jobID, message string) {
	line := time.Now().Format("15:04:05") + " " + message
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.job.ID != jobID {
		return
	}
	job := &s.current.job
	job.LogTail = append(job.LogTail, line)
	if len(job.LogTail) > maxLogTail {
		job.LogTail = append([]string(nil), job.LogTail[len(job.LogTail)-maxLogTail:]...)
	}
	_, _ = fmt.Fprintln(s.current.log, line)
}

func (s *Service) stateLocked() State {
	state := State{Config: s.config, AuthDir: s.resolvedAuthDirLocked(), AccountsPath: s.accountsPath, CookieDir: s.cookieDir}
	if s.current != nil {
		job := cloneJob(s.current.job)
		state.Job = &job
	} else if s.last != nil {
		job := cloneJob(*s.last)
		state.Job = &job
	}
	return state
}

func (s *Service) resolvedAuthDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolvedAuthDirLocked()
}

func (s *Service) resolvedAuthDirLocked() string {
	if s.authDir == nil {
		return ""
	}
	return strings.TrimSpace(s.authDir())
}

func (s *Service) probeConfigLocked(config Config) ProbeResult {
	browser := config.BrowserPath
	if browser == "" {
		browser = findBrowser()
	}
	stat, err := os.Stat(browser)
	checks := []ProbeCheck{
		{Name: "browser", OK: err == nil && !stat.IsDir(), Required: true, Detail: firstNonEmpty(browser, "未发现浏览器")},
		{Name: "auth_dir", OK: s.resolvedAuthDirLocked() != "", Required: true, Detail: s.resolvedAuthDirLocked()},
	}
	result := ProbeResult{OK: true, Checks: checks}
	for _, check := range checks {
		if check.Required && !check.OK {
			result.OK = false
		}
	}
	return result
}

func failedChecks(result ProbeResult) string {
	var failed []string
	for _, check := range result.Checks {
		if check.Required && !check.OK {
			failed = append(failed, check.Name+": "+check.Detail)
		}
	}
	return strings.Join(failed, "; ")
}

func cloneJob(job Job) Job {
	job.LogTail = append([]string(nil), job.LogTail...)
	job.Results = append([]AccountResult(nil), job.Results...)
	return job
}

func newJobID() (string, error) {
	buffer := make([]byte, 5)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102-150405-") + hex.EncodeToString(buffer), nil
}
