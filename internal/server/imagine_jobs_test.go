package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func imagineWriteCookie(t *testing.T, dir, name string) {
	t.Helper()
	cookieDir := filepath.Join(dir, "registrar", "cookies")
	if err := os.MkdirAll(cookieDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"cookies":[{"name":"sso","value":"tok-%s","domain":".grok.com"}]}`, name)
	if err := os.WriteFile(filepath.Join(cookieDir, name+".json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 画廊持久化：写入图片 + sidecar 后，刷新（重新扫描）仍能恢复元数据；
// 单张删除与整库清空均只作用于输出目录。
func TestImagineGalleryPersistence(t *testing.T) {
	dir := t.TempDir()
	s := &Server{Imagine: NewImagineEngine(dir)}

	if err := os.WriteFile(filepath.Join(s.Imagine.outputsDir, "a.jpg"), []byte("jpg-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := ImagineMeta{Prompt: "一只橘猫", Model: "grok-imagine-image", Aspect: "1:1", Account: "acct", Width: 1024, Height: 1024, CreatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(s.Imagine.outputsDir, "a.jpg.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// 无关文件不应出现在画廊里。
	if err := os.WriteFile(filepath.Join(s.Imagine.outputsDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleImagineGallery(rec, httptest.NewRequest(http.MethodGet, "/api/imagine/gallery", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("gallery status = %d", rec.Code)
	}
	var out struct {
		Items []ImagineGalleryItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("want 1 item, got %d: %#v", len(out.Items), out.Items)
	}
	item := out.Items[0]
	if item.File != "a.jpg" || item.URL != "/imagine-output/a.jpg" {
		t.Fatalf("item = %#v", item)
	}
	if item.Prompt != "一只橘猫" || item.Model != "grok-imagine-image" || item.Account != "acct" || item.Width != 1024 {
		t.Fatalf("metadata not restored: %#v", item)
	}

	// 单张删除。
	rec = httptest.NewRecorder()
	s.handleImagineGalleryDelete(rec, httptest.NewRequest(http.MethodPost, "/api/imagine/gallery/delete", bytes.NewBufferString(`{"file":"a.jpg"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.Imagine.outputsDir, "a.jpg")); !os.IsNotExist(err) {
		t.Fatalf("image file should be removed, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Imagine.outputsDir, "a.jpg.json")); !os.IsNotExist(err) {
		t.Fatalf("sidecar should be removed, err = %v", err)
	}

	// 路径穿越被中和：Base() 剥掉目录后目标不存在 → 404，绝不越出输出目录。
	rec = httptest.NewRecorder()
	s.handleImagineGalleryDelete(rec, httptest.NewRequest(http.MethodPost, "/api/imagine/gallery/delete", bytes.NewBufferString(`{"file":"../escape.jpg"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal want 404, got %d", rec.Code)
	}
	// 非图片扩展名直接拒绝。
	rec = httptest.NewRecorder()
	s.handleImagineGalleryDelete(rec, httptest.NewRequest(http.MethodPost, "/api/imagine/gallery/delete", bytes.NewBufferString(`{"file":"../../etc/passwd"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-image want 400, got %d", rec.Code)
	}

	// 清空。
	if err := os.WriteFile(filepath.Join(s.Imagine.outputsDir, "b.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleImagineGalleryClear(rec, httptest.NewRequest(http.MethodPost, "/api/imagine/gallery/clear", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d", rec.Code)
	}
	entries, err := os.ReadDir(s.Imagine.outputsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outputs dir should be empty, got %d entries", len(entries))
	}
}

// 异步任务：提交后立即返回 running；引擎失败（假账号连不上上游）后任务
// 进入 error 终态，fail_count 与 count 一致。
func TestImagineJobLifecycle(t *testing.T) {
	dir := t.TempDir()
	imagineWriteCookie(t, dir, "acct-a")
	s := &Server{Imagine: NewImagineEngine(dir)}

	body := `{"prompt":"a red cat","model":"grok-imagine-image","aspect_ratio":"1:1","count":2}`
	rec := httptest.NewRecorder()
	s.handleImagineGenerate(rec, httptest.NewRequest(http.MethodPost, "/api/imagine/generate", bytes.NewBufferString(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK  bool       `json:"ok"`
		Job imagineJob `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.OK || created.Job.ID == "" || created.Job.State != "running" || created.Job.Count != 2 {
		t.Fatalf("job = %#v", created.Job)
	}

	deadline := time.Now().Add(30 * time.Second)
	var final *imagineJob
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		s.handleImagineJobs(rec, httptest.NewRequest(http.MethodGet, "/api/imagine/jobs", nil))
		var out struct {
			Jobs []imagineJob `json:"jobs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Jobs) == 1 && out.Jobs[0].State != "running" {
			final = &out.Jobs[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("job did not finish in time")
	}
	if final.State != "error" || final.FailCount != 2 || final.OKCount != 0 {
		t.Fatalf("final job = %#v", final)
	}
	if final.Error == "" {
		t.Fatal("expected error message")
	}
}

// count 超上限被钳制到 imagineMaxBatch。
func TestImagineJobCountClamped(t *testing.T) {
	dir := t.TempDir()
	imagineWriteCookie(t, dir, "acct-a")
	s := &Server{Imagine: NewImagineEngine(dir)}
	job, _, err := s.startImagineJob("p", "", "", 99)
	if err != nil {
		t.Fatal(err)
	}
	if job.Count != imagineMaxBatch {
		t.Fatalf("count = %d, want %d", job.Count, imagineMaxBatch)
	}
}

// 引擎：busy 账号被跳过；exhausted 超过 TTL 自动恢复；热重载保留统计。
func TestImagineEngineParallelAndRecovery(t *testing.T) {
	dir := t.TempDir()
	imagineWriteCookie(t, dir, "acct-a")
	eng := NewImagineEngine(dir)
	if eng.AccountCount() != 1 {
		t.Fatalf("accounts = %d", eng.AccountCount())
	}
	acc := eng.accounts[0]

	// busy：waitForAccount 在 ctx 超时前不应返回该账号。
	acc.mu.Lock()
	acc.busy = true
	acc.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if got := eng.waitForAccount(ctx); got != nil {
		t.Fatal("busy account should not be returned")
	}
	cancel()
	acc.mu.Lock()
	acc.busy = false
	acc.mu.Unlock()
	if got := eng.waitForAccount(context.Background()); got != acc {
		t.Fatalf("want account after busy cleared, got %v", got)
	}

	// exhausted + TTL 过期 → 自动恢复。
	acc.mu.Lock()
	acc.exhausted = true
	acc.exhaustedAt = time.Now().Add(-2 * imagineExhaustedTTL)
	acc.mu.Unlock()
	eng.mu.Lock()
	got := eng.nextLocked()
	eng.mu.Unlock()
	if got != acc {
		t.Fatal("exhausted account should recover after TTL")
	}
	acc.mu.Lock()
	recovered := !acc.exhausted
	acc.mu.Unlock()
	if !recovered {
		t.Fatal("exhausted flag should be cleared")
	}

	// exhausted 未到 TTL → 不可用。
	acc.mu.Lock()
	acc.exhausted = true
	acc.exhaustedAt = time.Now()
	acc.mu.Unlock()
	eng.mu.Lock()
	got = eng.nextLocked()
	eng.mu.Unlock()
	if got != nil {
		t.Fatal("freshly exhausted account should be skipped")
	}

	// 热重载：新增账号可见，原账号统计保留。
	acc.mu.Lock()
	acc.successCount = 5
	acc.mu.Unlock()
	imagineWriteCookie(t, dir, "acct-b")
	eng.ReloadAccounts()
	if eng.AccountCount() != 2 {
		t.Fatalf("after reload accounts = %d", eng.AccountCount())
	}
	eng.mu.Lock()
	var preserved *imagineAccount
	for _, a := range eng.accounts {
		if a.id == "acct-a" {
			preserved = a
		}
	}
	eng.mu.Unlock()
	if preserved == nil {
		t.Fatal("acct-a missing after reload")
	}
	preserved.mu.Lock()
	success := preserved.successCount
	preserved.mu.Unlock()
	if success != 5 {
		t.Fatalf("stats not preserved: successCount = %d", success)
	}
}
