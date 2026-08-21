package grokpool

import (
	"context"
	"fmt"
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
)

type stickyBinding struct {
	AccountID string
	ExpiresAt time.Time
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
}

// sweepStickyLocked 删除全部过期绑定；仅在绑定数达到 stickySweepThreshold
// 时调用，把长期运行实例的内存占用限制在 TTL 内的活跃会话量级。
func (m *Manager) sweepStickyLocked(now time.Time) {
	for key, binding := range m.sticky {
		if now.After(binding.ExpiresAt) || binding.AccountID == "" {
			delete(m.sticky, key)
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
	for _, account := range m.state.Accounts {
		if exclude[account.ID] {
			continue
		}
		if accountAvailable(account) {
			accounts = append(accounts, account)
		}
	}
	m.mu.Unlock()

	if len(accounts) == 0 {
		return "", "", lostContinuation, fmtNoAccount()
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

func fmtNoAccount() error {
	return fmt.Errorf("Grok 号池没有可用账号")
}
