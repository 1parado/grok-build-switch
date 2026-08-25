package server

// imagine_jobs.go —— 生图异步任务与画廊持久化。
//
// POST /api/imagine/generate 立即返回任务 ID，生成在后台并发执行：
//   - 批量（count>1）时多张并行（引擎按账号排队，不会把同一账号并发打到上游）；
//   - 前端切换视图 / 刷新页面均不影响后台任务，回来后经 /api/imagine/jobs 续接进度；
//   - 每张成功图片带元数据 sidecar 落盘，画廊经 /api/imagine/gallery 刷新后仍在。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	imagineMaxBatch      = 6               // 单批最多张数（抽卡上限）
	imagineJobKeep       = 50              // 内存中保留的最近任务数
	imagineJobTTL        = time.Hour       // 已结束任务的保留时长
	imagineBatchTimeout  = 5 * time.Minute // 整批超时（含排队等待账号）
	imagineGalleryLimit  = 200             // 画廊单次最多返回条数
	imagineJobsPollAfter = 500 * time.Millisecond
)

// ImagineGalleryItem 是一张已落盘图片及其元数据。
type ImagineGalleryItem struct {
	URL       string    `json:"url"`
	File      string    `json:"file"`
	Prompt    string    `json:"prompt,omitempty"`
	Model     string    `json:"model,omitempty"`
	Aspect    string    `json:"aspect,omitempty"`
	Account   string    `json:"account,omitempty"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	SavedTo   string    `json:"saved_to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// imagineJob 是一次批量生图请求的完整生命周期。
type imagineJob struct {
	ID         string               `json:"id"`
	Prompt     string               `json:"prompt"`
	Model      string               `json:"model"`
	Aspect     string               `json:"aspect"`
	Count      int                  `json:"count"`
	State      string               `json:"state"` // running | done | error
	OKCount    int                  `json:"ok_count"`
	FailCount  int                  `json:"fail_count"`
	Images     []ImagineGalleryItem `json:"images"`
	Error      string               `json:"error,omitempty"`
	CreatedAt  time.Time            `json:"created_at"`
	FinishedAt time.Time            `json:"finished_at,omitempty"`
}

// imagineJobList 是进程内的任务表（按创建时间倒序返回）。
type imagineJobList struct {
	mu   sync.Mutex
	jobs []*imagineJob
}

func newImagineJobList() *imagineJobList {
	return &imagineJobList{}
}

func (l *imagineJobList) add(job *imagineJob) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jobs = append(l.jobs, job)
	// 清理：超过保留数量或已结束超 TTL 的任务。
	cutoff := time.Now().Add(-imagineJobTTL)
	kept := l.jobs[:0]
	for _, j := range l.jobs {
		if j.State == "running" || j.FinishedAt.IsZero() || j.FinishedAt.After(cutoff) {
			kept = append(kept, j)
		}
	}
	if len(kept) > imagineJobKeep {
		kept = kept[len(kept)-imagineJobKeep:]
	}
	l.jobs = kept
}

func (l *imagineJobList) snapshot() []imagineJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]imagineJob, 0, len(l.jobs))
	for i := len(l.jobs) - 1; i >= 0; i-- {
		out = append(out, *l.jobs[i])
	}
	return out
}

func (l *imagineJobList) get(id string) *imagineJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, j := range l.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// recordResult 记录一张图的成功/失败；返回任务当前快照。
func (l *imagineJobList) recordResult(id string, item ImagineGalleryItem, ok bool, errMsg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, j := range l.jobs {
		if j.ID != id {
			continue
		}
		if ok {
			j.OKCount++
			j.Images = append(j.Images, item)
		} else {
			j.FailCount++
			j.Error = errMsg
		}
		if j.OKCount+j.FailCount >= j.Count {
			j.State = "done"
			if j.OKCount == 0 {
				j.State = "error"
			}
			j.FinishedAt = time.Now()
		}
		return
	}
}

// ---- Server 侧接线 ----

func (s *Server) jobList() *imagineJobList {
	s.imagineMu.Lock()
	defer s.imagineMu.Unlock()
	if s.imagineJobs == nil {
		s.imagineJobs = newImagineJobList()
	}
	return s.imagineJobs
}

// startImagineJob 校验并创建一个后台批量生图任务。
// 返回 (job, httpStatus, err)：校验失败时 err 非空。
func (s *Server) startImagineJob(prompt, model, aspect string, count int) (*imagineJob, int, error) {
	if s.Imagine == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("生图引擎未初始化")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("prompt 不能为空")
	}
	if model == "" {
		model = "grok-imagine-image"
	}
	if aspect == "" {
		aspect = "1:1"
	}
	if count < 1 {
		count = 1
	}
	if count > imagineMaxBatch {
		count = imagineMaxBatch
	}
	// 热加载账号：新注册的账号无需重启即可用。
	s.Imagine.ReloadAccounts()
	if s.Imagine.AccountCount() == 0 {
		return nil, http.StatusBadGateway, fmt.Errorf("未找到任何可用账号（registrar/cookies 为空或无效）")
	}
	job := &imagineJob{
		ID:        "img-" + randomHex(6),
		Prompt:    prompt,
		Model:     model,
		Aspect:    aspect,
		Count:     count,
		State:     "running",
		CreatedAt: time.Now(),
	}
	s.jobList().add(job)
	go s.runImagineJob(job)
	return job, 0, nil
}

func (s *Server) runImagineJob(job *imagineJob) {
	ctx, cancel := context.WithTimeout(context.Background(), imagineBatchTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < job.Count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := s.Imagine.Generate(ctx, job.Prompt, job.Model, job.Aspect)
			item := ImagineGalleryItem{
				Prompt:    job.Prompt,
				Model:     res.ModelName,
				Aspect:    job.Aspect,
				Account:   res.Account,
				Width:     res.Width,
				Height:    res.Height,
				CreatedAt: time.Now().UTC(),
			}
			if res.OK && len(res.Images) > 0 {
				file := filepath.Base(res.Images[0])
				item.URL = res.Images[0]
				item.File = file
				// 与旧同步接口一致：额外复制一份到系统下载目录。
				if dlPath, dlErr := saveImageToDownloads(filepath.Join(s.Imagine.outputsDir, file)); dlErr == nil {
					item.SavedTo = dlPath
				}
				s.jobList().recordResult(job.ID, item, true, "")
				return
			}
			errMsg := orEmpty(res.ErrMsg, "生图失败")
			if res.ErrCode != "" {
				errMsg = res.ErrCode + ": " + errMsg
			}
			s.jobList().recordResult(job.ID, item, false, errMsg)
		}()
	}
	wg.Wait()
}

// handleImagineJobs GET /api/imagine/jobs —— 前端轮询任务进度。
func (s *Server) handleImagineJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, map[string]any{"jobs": s.jobList().snapshot()})
}

// ---- 画廊持久化 ----

func imagineImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

// handleImagineGallery GET /api/imagine/gallery —— 扫描输出目录恢复画廊。
func (s *Server) handleImagineGallery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Imagine == nil {
		writeJSON(w, map[string]any{"items": []ImagineGalleryItem{}})
		return
	}
	writeJSON(w, map[string]any{"items": s.imagineGalleryItems()})
}

func (s *Server) imagineGalleryItems() []ImagineGalleryItem {
	entries, err := os.ReadDir(s.Imagine.outputsDir)
	if err != nil {
		return []ImagineGalleryItem{}
	}
	type entryInfo struct {
		name string
		mod  time.Time
		meta ImagineMeta
	}
	items := make([]entryInfo, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || !imagineImageExt(ent.Name()) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		entry := entryInfo{name: ent.Name(), mod: info.ModTime()}
		if raw, err := os.ReadFile(filepath.Join(s.Imagine.outputsDir, ent.Name()+".json")); err == nil {
			_ = json.Unmarshal(raw, &entry.meta)
		}
		items = append(items, entry)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	if len(items) > imagineGalleryLimit {
		items = items[:imagineGalleryLimit]
	}
	out := make([]ImagineGalleryItem, 0, len(items))
	for _, it := range items {
		item := ImagineGalleryItem{
			URL:       "/" + imagineOutputURLPath + "/" + it.name,
			File:      it.name,
			Prompt:    it.meta.Prompt,
			Model:     it.meta.Model,
			Aspect:    it.meta.Aspect,
			Account:   it.meta.Account,
			Width:     it.meta.Width,
			Height:    it.meta.Height,
			CreatedAt: it.meta.CreatedAt,
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = it.mod
		}
		out = append(out, item)
	}
	return out
}

// handleImagineGalleryClear POST /api/imagine/gallery/clear —— 删除全部输出。
func (s *Server) handleImagineGalleryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Imagine == nil {
		writeError(w, fmt.Errorf("生图引擎未初始化"), http.StatusInternalServerError)
		return
	}
	entries, err := os.ReadDir(s.Imagine.outputsDir)
	if err == nil {
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			_ = os.Remove(filepath.Join(s.Imagine.outputsDir, ent.Name()))
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleImagineGalleryDelete POST /api/imagine/gallery/delete —— 删除单张（含 sidecar）。
func (s *Server) handleImagineGalleryDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Imagine == nil {
		writeError(w, fmt.Errorf("生图引擎未初始化"), http.StatusInternalServerError)
		return
	}
	var req struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	name := filepath.Base(strings.TrimSpace(req.File))
	if name == "" || name == "." || !imagineImageExt(name) {
		writeError(w, fmt.Errorf("无效的文件名"), http.StatusBadRequest)
		return
	}
	target := filepath.Join(s.Imagine.outputsDir, name)
	if _, err := os.Stat(target); err != nil {
		writeError(w, fmt.Errorf("文件不存在"), http.StatusNotFound)
		return
	}
	if err := os.Remove(target); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	_ = os.Remove(target + ".json")
	writeJSON(w, map[string]bool{"ok": true})
}
