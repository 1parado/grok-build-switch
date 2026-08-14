package server

// relogin.go — 「刷新 Cookie」接口：按号池账号重新登录 x.ai 并重新铸造 CPA。
//
// 每个账号复用 registrar.ReloginAccount（浏览器登录 → 新 sso → 设备码铸造），
// 成功后把新 CPA 凭据重新导入号池，使 expires_at 等信息即时更新。
// 提供：
//   - POST /api/grok-pool/refresh-cookie  body: {"ids":["..."], ...}（ids 为空=全部）
//   - GET  /api/grok-pool/refresh-cookie/{id}  任务状态（前端轮询）
//
// 账号密码从 registrar 账本（accounts_cli.txt）按邮箱查找；找不到的账号标记 failed/skipped。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/grokpool"
	"grok_switch/internal/registrar"
)

const (
	maxReloginEntryLines = 300
	reloginJobTTL        = time.Hour
)

type reloginEntry struct {
	ID         string   `json:"id"`
	Email      string   `json:"email"`
	Status     string   `json:"status"` // queued | running | success | failed
	Error      string   `json:"error,omitempty"`
	MintMethod string   `json:"mint_method,omitempty"`
	AuthFile   string   `json:"auth_file,omitempty"`
	CookieFile string   `json:"cookie_file,omitempty"`
	Lines      []string `json:"lines,omitempty"`
}

type reloginRun struct {
	mu         sync.Mutex
	ID         string
	CreatedAt  time.Time
	FinishedAt time.Time
	Running    bool
	Total      int
	Done       int
	Entries    []*reloginEntry
	cancel     context.CancelFunc
}

func (run *reloginRun) snapshot() map[string]any {
	run.mu.Lock()
	defer run.mu.Unlock()
	entries := make([]reloginEntry, len(run.Entries))
	for i, e := range run.Entries {
		cp := *e
		cp.Lines = append([]string(nil), e.Lines...)
		entries[i] = cp
	}
	return map[string]any{
		"id":          run.ID,
		"created_at":  run.CreatedAt,
		"finished_at": run.FinishedAt,
		"running":     run.Running,
		"total":       run.Total,
		"done":        run.Done,
		"entries":     entries,
	}
}

func (s *Server) newReloginRun() *reloginRun {
	s.reloginMu.Lock()
	defer s.reloginMu.Unlock()
	if s.reloginJobs == nil {
		s.reloginJobs = map[string]*reloginRun{}
	}
	s.reloginSeq++
	run := &reloginRun{ID: fmt.Sprintf("r%d", s.reloginSeq), CreatedAt: time.Now(), Running: true}
	s.reloginJobs[run.ID] = run
	for id, job := range s.reloginJobs {
		if !job.Running && time.Since(job.FinishedAt) > reloginJobTTL {
			delete(s.reloginJobs, id)
		}
	}
	return run
}

func (s *Server) reloginRunByID(id string) *reloginRun {
	s.reloginMu.Lock()
	defer s.reloginMu.Unlock()
	return s.reloginJobs[id]
}

// handleGrokPoolRefreshCookie 启动「刷新 cookie」任务（单个或批量）。
func (s *Server) handleGrokPoolRefreshCookie(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.GrokPool == nil || s.Registrar == nil {
		writeError(w, fmt.Errorf("号池或注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req)
	}
	want := map[string]bool{}
	for _, id := range req.IDs {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	var targets []grokpool.Account
	for _, acc := range s.GrokPool.Status().Accounts {
		if strings.TrimSpace(acc.Email) == "" {
			continue
		}
		if len(want) > 0 && !want[acc.ID] {
			continue
		}
		targets = append(targets, acc)
	}
	if len(targets) == 0 {
		writeError(w, fmt.Errorf("没有可刷新的账号（号池为空，或指定账号不存在）"), http.StatusBadRequest)
		return
	}
	run := s.newReloginRun()
	run.Total = len(targets)
	for _, acc := range targets {
		run.Entries = append(run.Entries, &reloginEntry{ID: acc.ID, Email: acc.Email, Status: "queued"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go s.executeReloginRun(ctx, run)
	writeJSONStatus(w, map[string]any{"id": run.ID, "total": run.Total}, http.StatusAccepted)
}

// handleGrokPoolRefreshCookieStatus 返回刷新任务进度（前端轮询）。
func (s *Server) handleGrokPoolRefreshCookieStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/grok-pool/refresh-cookie/"), "/")
	if id == "" {
		writeError(w, fmt.Errorf("缺少刷新任务 id"), http.StatusBadRequest)
		return
	}
	run := s.reloginRunByID(id)
	if run == nil {
		writeError(w, fmt.Errorf("刷新任务不存在或已过期: %s", id), http.StatusNotFound)
		return
	}
	writeJSON(w, run.snapshot())
}

func (s *Server) executeReloginRun(ctx context.Context, run *reloginRun) {
	defer func() {
		run.mu.Lock()
		run.Running = false
		run.FinishedAt = time.Now()
		run.mu.Unlock()
		s.changed()
	}()
	for _, entry := range run.Entries {
		if ctx.Err() != nil {
			return
		}
		func() {
			run.mu.Lock()
			entry.Status = "running"
			run.mu.Unlock()
		}()
		s.changed()

		logFn := func(line string) {
			line = strings.TrimSpace(line)
			if line == "" {
				return
			}
			run.mu.Lock()
			defer run.mu.Unlock()
			entry.Lines = append(entry.Lines, line)
			if len(entry.Lines) > maxReloginEntryLines {
				entry.Lines = entry.Lines[len(entry.Lines)-maxReloginEntryLines:]
			}
		}

		logFn("开始刷新 " + entry.Email)
		password, ok := s.Registrar.AccountPassword(entry.Email)
		if !ok {
			run.mu.Lock()
			entry.Status = "failed"
			entry.Error = "registrar 账本（accounts_cli.txt）中没有该邮箱的密码，跳过"
			run.Done++
			run.mu.Unlock()
			logFn("失败：" + entry.Error)
			continue
		}

		config := s.Registrar.Get().Config
		authDir := s.GrokPool.ResolvedAuthDir()
		cookieDir := s.Registrar.CookieDir()
		reloginCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		// 两段式：优先用文件里的 refresh_token 直接续期，失败再回退浏览器重新登录。
		res, err := registrar.RefreshAccount(reloginCtx, config, entry.Email, password, authDir, cookieDir, logFn)
		cancel()

		if err == nil {
			run.mu.Lock()
			entry.Status = "success"
			entry.MintMethod = res.MintMethod
			entry.AuthFile = res.AuthFile
			entry.CookieFile = res.CookieFile
			run.Done++
			run.mu.Unlock()
		} else {
			entryErr := pickFirst(res.Error, err.Error())
			run.mu.Lock()
			entry.Status = "failed"
			entry.Error = entryErr
			run.Done++
			run.mu.Unlock()
			logFn("失败：" + entryErr)
		}

		if err == nil && res.AuthFile != "" {
			if data, rerr := os.ReadFile(res.AuthFile); rerr == nil {
				if _, ierr := s.GrokPool.Import([]grokpool.ImportFile{{Name: filepath.Base(res.AuthFile), Content: string(data)}}); ierr != nil {
					logFn("号池重新导入失败: " + ierr.Error())
				}
			}
		}
		s.changed()
	}
}

func pickFirst(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
