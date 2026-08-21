package grokpool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	stickyTTL = time.Hour
	// stickySweepThreshold 触发过期绑定的全量清扫；正常情况下条目由
	// lookup 惰性删除，但从未再次被查到的 resp_/sess_ 绑定只能靠这里回收，
	// 否则长期运行的实例会随每个成功响应无限累积。
	stickySweepThreshold = 4096
	// stickyFlushInterval 限制粘性绑定落盘频率：绑定随每个成功响应产生，
	// 不能每条都写盘，靠脏标记 + 周期刷盘 + 关闭时刷盘兜底。
	stickyFlushInterval = 15 * time.Second
)

// stickyBinding 的字段带 JSON 标签，便于直接持久化到 sticky.json。
type stickyBinding struct {
	AccountID string    `json:"account_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type stickyPersistFile struct {
	Version  int                      `json:"version"`
	Bindings map[string]stickyBinding `json:"bindings"`
}

func (m *Manager) lookupStickyLocked(key string, now time.Time) string {
	if m.sticky == nil || strings.TrimSpace(key) == "" {
		return ""
	}
	binding, ok := m.sticky[key]
	if !ok {
		return ""
	}
	if now.After(binding.ExpiresAt) || binding.AccountID == "" {
		delete(m.sticky, key)
		m.stickyDirty = true
		return ""
	}
	return binding.AccountID
}

func (m *Manager) bindStickyLocked(key, accountID string, now time.Time) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(accountID) == "" {
		return
	}
	if m.sticky == nil {
		m.sticky = make(map[string]stickyBinding)
	}
	if len(m.sticky) >= stickySweepThreshold {
		m.sweepStickyLocked(now)
	}
	m.sticky[key] = stickyBinding{AccountID: accountID, ExpiresAt: now.Add(stickyTTL)}
	m.stickyDirty = true
}

// sweepStickyLocked 删除全部过期绑定；仅在绑定数达到 stickySweepThreshold
// 时调用，把长期运行实例的内存占用限制在 TTL 内的活跃会话量级。
func (m *Manager) sweepStickyLocked(now time.Time) {
	removed := false
	for key, binding := range m.sticky {
		if now.After(binding.ExpiresAt) || binding.AccountID == "" {
			delete(m.sticky, key)
			removed = true
		}
	}
	if removed {
		m.stickyDirty = true
	}
}

// loadSticky 从数据目录恢复粘性绑定（应用重启/自动更新后进行中的会话
// 不掉线），过期条目在加载时剪除。文件缺失或损坏按无绑定处理。
func (m *Manager) loadSticky() {
	raw, err := os.ReadFile(m.stickyPath)
	if err != nil {
		return
	}
	var file stickyPersistFile
	if json.Unmarshal(raw, &file) != nil {
		return
	}
	now := time.Now()
	bindings := make(map[string]stickyBinding, len(file.Bindings))
	for key, binding := range file.Bindings {
		if strings.TrimSpace(key) == "" || binding.AccountID == "" || now.After(binding.ExpiresAt) {
			continue
		}
		bindings[key] = binding
	}
	m.mu.Lock()
	if m.sticky == nil {
		m.sticky = bindings
	} else {
		for key, binding := range bindings {
			m.sticky[key] = binding
		}
	}
	m.stickyDirty = false
	m.mu.Unlock()
}

// flushSticky 把脏标记的绑定快照原子写盘；并发绑定期间拿到的快照可能
// 略旧，由下一轮刷盘补齐。写失败时恢复脏标记等待重试。
func (m *Manager) flushSticky() error {
	m.mu.Lock()
	if !m.stickyDirty {
		m.mu.Unlock()
		return nil
	}
	bindings := make(map[string]stickyBinding, len(m.sticky))
	for key, binding := range m.sticky {
		bindings[key] = binding
	}
	m.stickyDirty = false
	path := m.stickyPath
	m.mu.Unlock()

	data, err := json.Marshal(stickyPersistFile{Version: poolVersion, Bindings: bindings})
	if err != nil {
		return err
	}
	if err := atomicWrite(path, append(data, '\n')); err != nil {
		m.mu.Lock()
		m.stickyDirty = true
		m.mu.Unlock()
		return err
	}
	return nil
}

// stickyFlushLoop 周期把脏绑定落盘；停止时做最后一次刷盘。
func (m *Manager) stickyFlushLoop() {
	defer m.runWG.Done()
	ticker := time.NewTicker(stickyFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			_ = m.flushSticky()
			return
		case <-ticker.C:
			_ = m.flushSticky()
		}
	}
}

// BindSession pins a conversation/session identifier to an account after a
// successful upstream response. Subsequent turns with the same id stay on that
// account so previous_response_id and encrypted reasoning remain valid.
func (m *Manager) BindSession(sessionID, accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindStickyLocked("sess:"+strings.TrimSpace(sessionID), accountID, time.Now())
}

// BindResponse pins a Responses API id to the account that produced it.
func (m *Manager) BindResponse(responseID, accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindStickyLocked("resp:"+strings.TrimSpace(responseID), accountID, time.Now())
}

func (m *Manager) accountAvailableByIDLocked(id string) bool {
	for _, account := range m.state.Accounts {
		if account.ID == id {
			return accountAvailable(account)
		}
	}
	return false
}

// NextTokenSticky selects a pool token with persisted affinity.
// previousResponseID is the strongest pin (encrypted reasoning / response
// continuation). sessionID covers Grok CLI conversation headers. lostContinuation
// is true when previous_response_id was bound to an account that is no longer
// available — callers should drop previous_response_id before sending to a
// replacement account.
func (m *Manager) NextTokenSticky(ctx context.Context, sessionID, previousResponseID string) (token, accountID string, lostContinuation bool, err error) {
	return m.NextTokenStickyExcluding(ctx, sessionID, previousResponseID, nil)
}

// NextTokenStickyExcluding 与 NextTokenSticky 相同，但跳过 exclude 中的账号
// （透明故障转移：本轮请求里已经失败过的号不再复用）。被排除的续接绑定
// 视为 lostContinuation；全部候选被排除时返回与无可用账号相同的错误。
func (m *Manager) NextTokenStickyExcluding(ctx context.Context, sessionID, previousResponseID string, exclude map[string]bool) (token, accountID string, lostContinuation bool, err error) {
	sessionID = strings.TrimSpace(sessionID)
	previousResponseID = strings.TrimSpace(previousResponseID)

	m.mu.Lock()
	now := time.Now()
	preferred := ""
	if previousResponseID != "" {
		if id := m.lookupStickyLocked("resp:"+previousResponseID, now); id != "" {
			if exclude[id] || !m.accountAvailableByIDLocked(id) {
				lostContinuation = true
			} else {
				preferred = id
			}
		}
	}
	if preferred == "" && sessionID != "" {
		if id := m.lookupStickyLocked("sess:"+sessionID, now); id != "" && !exclude[id] && m.accountAvailableByIDLocked(id) {
			preferred = id
		}
	}
	accounts := make([]Account, 0, len(m.state.Accounts))
	unavailable := make(map[string]int)
	for _, account := range m.state.Accounts {
		if exclude[account.ID] {
			continue
		}
		if !accountAvailable(account) {
			unavailable[stickyUnavailableReason(account)]++
			continue
		}
		accounts = append(accounts, account)
	}
	total := len(m.state.Accounts)
	m.mu.Unlock()

	if len(accounts) == 0 {
		return "", "", lostContinuation, fmtNoAvailableAccount(total, unavailable, len(exclude))
	}
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })

	ordered := make([]Account, 0, len(accounts))
	if preferred != "" {
		for _, account := range accounts {
			if account.ID == preferred {
				ordered = append(ordered, account)
				break
			}
		}
	}
	start := int(m.roundRobin.Add(1)-1) % len(accounts)
	for offset := 0; offset < len(accounts); offset++ {
		account := accounts[(start+offset)%len(accounts)]
		if preferred != "" && account.ID == preferred {
			continue
		}
		ordered = append(ordered, account)
	}

	var failures []string
	for _, account := range ordered {
		got, tokenErr := m.accountStore(account.ID).Token(ctx)
		if tokenErr == nil {
			return got, account.ID, lostContinuation, nil
		}
		failures = append(failures, firstNonEmpty(account.Email, account.ID)+": "+tokenErr.Error())
		m.recordTokenFailure(account.ID, tokenErr)
	}
	return "", "", lostContinuation, fmt.Errorf("号池账号 token 均不可用: %s", strings.Join(failures, "; "))
}

// stickyUnavailableReason 把不可用账号归到用户可读的原因桶；
// 手动停用优先于健康分类（两者可能同时成立，停用是用户显式意图）。
func stickyUnavailableReason(account Account) string {
	if account.Disabled {
		return "已手动停用"
	}
	switch account.Classification {
	case "quota_exhausted":
		return "免费额度耗尽"
	case "reauth":
		return "待重新登录"
	case "permission_denied":
		return "对话权限被拒"
	default:
		return "暂不可用"
	}
}

// fmtNoAvailableAccount 生成可操作的池耗尽错误：区分空池与全部不可用，
// 给出原因分布；故障转移耗尽候选时额外提示本轮已尝试全部可用账号。
func fmtNoAvailableAccount(total int, unavailable map[string]int, excluded int) error {
	if total == 0 {
		return fmt.Errorf("Grok 号池为空，请先在号池页导入 Grok auth 账号")
	}
	parts := make([]string, 0, len(unavailable))
	for _, reason := range []string{"已手动停用", "免费额度耗尽", "待重新登录", "对话权限被拒", "暂不可用"} {
		if count := unavailable[reason]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d 个%s", count, reason))
		}
	}
	message := fmt.Sprintf("Grok 号池没有可用账号：共 %d 个账号", total)
	if len(parts) > 0 {
		message += "（" + strings.Join(parts, "、") + "）"
	}
	message += "，请补充新账号或运行巡检"
	if excluded > 0 {
		message += fmt.Sprintf("；本轮请求已尝试全部 %d 个可用账号", excluded)
	}
	return fmt.Errorf("%s", message)
}
