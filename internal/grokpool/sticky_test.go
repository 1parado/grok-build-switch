package grokpool

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStickySessionPinsTheSameAccount(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-b","expired":%q,"email":"b@example.com"}`, expiry)},
	}); err != nil {
		t.Fatal(err)
	}

	firstToken, firstID, lost, err := manager.NextTokenSticky(t.Context(), "conv-keep", "")
	if err != nil || lost || firstToken == "" || firstID == "" {
		t.Fatalf("first NextTokenSticky() = %q %q lost=%v err=%v", firstToken, firstID, lost, err)
	}
	manager.BindSession("conv-keep", firstID)

	for i := 0; i < 8; i++ {
		token, id, lost, err := manager.NextTokenSticky(t.Context(), "conv-keep", "")
		if err != nil || lost {
			t.Fatalf("repeat %d: %v lost=%v", i, err, lost)
		}
		if id != firstID || token != firstToken {
			t.Fatalf("repeat %d switched account %q -> %q", i, firstID, id)
		}
	}

	_, otherID, _, err := manager.NextTokenSticky(t.Context(), "conv-other", "")
	if err != nil {
		t.Fatal(err)
	}
	if otherID == "" {
		t.Fatal("empty other account")
	}
}

func TestPreviousResponseIDPinsAccountAndReportsLostContinuation(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-b","expired":%q,"email":"b@example.com"}`, expiry)},
	}); err != nil {
		t.Fatal(err)
	}
	_, firstID, _, err := manager.NextTokenSticky(t.Context(), "sess-1", "")
	if err != nil {
		t.Fatal(err)
	}
	manager.BindResponse("resp_keep", firstID)

	token, id, lost, err := manager.NextTokenSticky(t.Context(), "different-session", "resp_keep")
	if err != nil || lost {
		t.Fatalf("previous_response sticky failed: token=%q id=%q lost=%v err=%v", token, id, lost, err)
	}
	if id != firstID {
		t.Fatalf("previous_response_id did not pin account: got %q want %q", id, firstID)
	}

	if _, err := manager.SetDisabled(firstID, true); err != nil {
		t.Fatal(err)
	}
	_, nextID, lost, err := manager.NextTokenSticky(t.Context(), "different-session", "resp_keep")
	if err != nil {
		t.Fatal(err)
	}
	if !lost {
		t.Fatal("expected lostContinuation when pinned account is disabled")
	}
	if nextID == firstID {
		t.Fatal("disabled sticky account was still selected")
	}
}

func TestBindSessionSweepsExpiredBindings(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	manager.mu.Lock()
	for i := 0; i < stickySweepThreshold+10; i++ {
		key := fmt.Sprintf("resp:%d", i)
		if i%2 == 0 {
			manager.sticky[key] = stickyBinding{AccountID: "acc", ExpiresAt: now.Add(-time.Minute)}
		} else {
			manager.sticky[key] = stickyBinding{AccountID: "acc", ExpiresAt: now.Add(time.Hour)}
		}
	}
	manager.bindStickyLocked("resp:trigger", "acc", now)
	liveCount := 0
	for key, binding := range manager.sticky {
		if now.After(binding.ExpiresAt) {
			t.Fatalf("expired binding %q survived sweep", key)
		}
		liveCount++
	}
	manager.mu.Unlock()

	// 一半过期 + 新写入 1 条；清扫后应只剩活跃条目。
	if want := stickySweepThreshold/2 + 6; liveCount != want {
		t.Fatalf("after sweep live = %d, want %d", liveCount, want)
	}
}

func TestNextTokenStickyExcludingSkipsFailedAccounts(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-b","expired":%q,"email":"b@example.com"}`, expiry)},
	}); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if len(status.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(status.Accounts))
	}
	firstID := status.Accounts[0].ID
	secondID := status.Accounts[1].ID

	token, id, lost, err := manager.NextTokenStickyExcluding(t.Context(), "", "", map[string]bool{firstID: true})
	if err != nil || lost || token == "" {
		t.Fatalf("NextTokenStickyExcluding() = %q %q lost=%v err=%v", token, id, lost, err)
	}
	if id != secondID {
		t.Fatalf("excluded account still selected: got %q want %q", id, secondID)
	}

	if _, _, _, err := manager.NextTokenStickyExcluding(t.Context(), "", "", map[string]bool{firstID: true, secondID: true}); err == nil {
		t.Fatal("expected error when every account is excluded")
	}
}

func TestNextTokenStickyExcludingReportsLostContinuationForExcludedPin(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-b","expired":%q,"email":"b@example.com"}`, expiry)},
	}); err != nil {
		t.Fatal(err)
	}
	_, pinnedID, _, err := manager.NextTokenSticky(t.Context(), "sess-x", "")
	if err != nil {
		t.Fatal(err)
	}
	manager.BindResponse("resp_fail", pinnedID)

	_, nextID, lost, err := manager.NextTokenStickyExcluding(t.Context(), "sess-x", "resp_fail", map[string]bool{pinnedID: true})
	if err != nil {
		t.Fatal(err)
	}
	if !lost {
		t.Fatal("expected lostContinuation when pinned response account is excluded")
	}
	if nextID == pinnedID {
		t.Fatal("excluded pinned account was selected")
	}
}

func TestStickyBindingsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	m1, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := m1.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-b","expired":%q,"email":"b@example.com"}`, expiry)},
	}); err != nil {
		t.Fatal(err)
	}
	_, boundID, _, err := m1.NextTokenSticky(t.Context(), "sess-restart", "")
	if err != nil || boundID == "" {
		t.Fatalf("NextTokenSticky() = %q err=%v", boundID, err)
	}
	m1.BindSession("sess-restart", boundID)
	m1.BindResponse("resp_restart", boundID)
	// 直接注入一条过期绑定，验证加载时剪除。
	m1.mu.Lock()
	m1.sticky["resp:stale"] = stickyBinding{AccountID: boundID, ExpiresAt: time.Now().Add(-time.Minute)}
	m1.stickyDirty = true
	m1.mu.Unlock()
	if err := m1.flushSticky(); err != nil {
		t.Fatal(err)
	}

	m2, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, id, lost, err := m2.NextTokenSticky(t.Context(), "sess-restart", "")
	if err != nil || lost || token == "" {
		t.Fatalf("after restart NextTokenSticky() = %q lost=%v err=%v", id, lost, err)
	}
	if id != boundID {
		t.Fatalf("session affinity lost after restart: got %q want %q", id, boundID)
	}
	_, respID, lostResp, err := m2.NextTokenSticky(t.Context(), "", "resp_restart")
	if err != nil || lostResp || respID != boundID {
		t.Fatalf("response affinity lost after restart: id=%q lost=%v err=%v", respID, lostResp, err)
	}
	m2.mu.Lock()
	_, staleExists := m2.sticky["resp:stale"]
	m2.mu.Unlock()
	if staleExists {
		t.Fatal("expired binding survived reload")
	}
}

func TestNoAvailableAccountErrorExplainsReasons(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Import([]ImportFile{
		{Name: "a.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-a","expired":%q,"email":"a@example.com"}`, expiry)},
		{Name: "b.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-b","expired":%q,"email":"b@example.com"}`, expiry)},
		{Name: "c.json", Content: fmt.Sprintf(`{"type":"xai","access_token":"token-c","expired":%q,"email":"c@example.com"}`, expiry)},
	}); err != nil {
		t.Fatal(err)
	}
	ids := manager.Status().Accounts
	if _, err := manager.SetDisabled(ids[0].ID, true); err != nil {
		t.Fatal(err)
	}
	manager.ObserveResponse(ids[1].ID, 429, `{"code":"free-usage-exhausted","message":"You have used all the included free usage"}`)
	manager.ObserveResponse(ids[2].ID, 401, `{"code":"unauthorized","error":"token has been invalidated"}`)

	_, _, _, err = manager.NextTokenSticky(t.Context(), "", "")
	if err == nil {
		t.Fatal("expected error when every account is unavailable")
	}
	message := err.Error()
	for _, want := range []string{"共 3 个账号", "1 个已手动停用", "1 个免费额度耗尽", "1 个待重新登录"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q missing %q", message, want)
		}
	}
}
