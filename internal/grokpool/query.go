package grokpool

import (
	"sort"
	"strings"
	"time"
)

// QueryOpts filters and paginates the account list returned to the UI. With
// thousands of accounts the full Status() payload is too large to render at
// once, so the account list is fetched incrementally via QueryAccounts.
type QueryOpts struct {
	// Query matches (case-insensitive, substring) against email, file name and id.
	Query string
	// Classifications, when non-empty, restricts results to the given
	// classifications (e.g. "healthy", "quota_exhausted"). Empty matches all.
	Classifications []string
	// Disabled filters by the disabled flag. Nil = ignore; &true/&false = filter.
	Disabled *bool
	// Sort is one of: "recent" (last_inspected desc, default), "imported"
	// (imported_at desc), "email" (alphabetical), "expires" (expires_at asc).
	Sort string
	// Page is 1-based.
	Page int
	// PageSize defaults to 100 and is clamped to 500.
	PageSize int
}

// AccountPage is the paginated account response.
type AccountPage struct {
	Summary  Summary   `json:"summary"`
	Accounts []Account `json:"accounts"`
	Page     int       `json:"page"`
	PageSize int       `json:"page_size"`
	Total    int       `json:"total"`
}

const (
	defaultQueryPageSize = 100
	maxQueryPageSize     = 500
)

// QueryAccounts returns a filtered, sorted and paginated slice of accounts
// together with the global summary (computed over ALL accounts, ignoring
// filters, so the header stats always reflect the whole pool).
func (m *Manager) QueryAccounts(opts QueryOpts) AccountPage {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := append([]Account(nil), m.state.Accounts...)
	summary := summarize(all)

	normalizedClasses := normalizeClassFilter(opts.Classifications)
	query := strings.ToLower(strings.TrimSpace(opts.Query))

	filtered := make([]Account, 0, len(all))
	for _, account := range all {
		if !matchesClassFilter(account, normalizedClasses) {
			continue
		}
		if opts.Disabled != nil && account.Disabled != *opts.Disabled {
			continue
		}
		if query != "" && !matchesQuery(account, query) {
			continue
		}
		filtered = append(filtered, account)
	}

	sortAccountsBy(filtered, opts.Sort)

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = defaultQueryPageSize
	}
	if pageSize > maxQueryPageSize {
		pageSize = maxQueryPageSize
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}
	total := len(filtered)
	if page > 1 && (page-1)*pageSize >= total {
		page = 1
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	if start > end {
		end = start
	}
	pageItems := append([]Account(nil), filtered[start:end]...)
	if pageItems == nil {
		pageItems = []Account{}
	}
	return AccountPage{
		Summary:  summary,
		Accounts: pageItems,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
	}
}

func normalizeClassFilter(classifications []string) map[string]bool {
	out := make(map[string]bool, len(classifications))
	for _, class := range classifications {
		class = strings.TrimSpace(class)
		if class != "" {
			out[strings.ToLower(class)] = true
		}
	}
	return out
}

func matchesClassFilter(account Account, classes map[string]bool) bool {
	if len(classes) == 0 {
		return true
	}
	classification := strings.TrimSpace(account.Classification)
	if classification == "" {
		classification = "uninspected"
	}
	// "disabled" is a virtual classification so the UI can filter disabled
	// accounts regardless of their health state.
	if classes["disabled"] && account.Disabled {
		return true
	}
	if classes[classification] {
		return true
	}
	// "abnormal" groups every non-healthy, non-uninspected account.
	if classes["abnormal"] && accountAbnormal(account) {
		return true
	}
	return false
}

func matchesQuery(account Account, query string) bool {
	if strings.Contains(strings.ToLower(account.Email), query) {
		return true
	}
	if strings.Contains(strings.ToLower(account.FileName), query) {
		return true
	}
	if strings.Contains(strings.ToLower(account.ID), query) {
		return true
	}
	return false
}

func sortAccountsBy(accounts []Account, sortKey string) {
	switch strings.ToLower(strings.TrimSpace(sortKey)) {
	case "imported":
		sort.SliceStable(accounts, func(i, j int) bool {
			return accounts[i].ImportedAt.After(accounts[j].ImportedAt)
		})
	case "email":
		sort.SliceStable(accounts, func(i, j int) bool {
			left := strings.ToLower(firstNonEmpty(accounts[i].Email, accounts[i].FileName, accounts[i].ID))
			right := strings.ToLower(firstNonEmpty(accounts[j].Email, accounts[j].FileName, accounts[j].ID))
			return left < right
		})
	case "expires":
		sort.SliceStable(accounts, func(i, j int) bool {
			ai, aj := accounts[i].ExpiresAt, accounts[j].ExpiresAt
			if ai.IsZero() {
				ai = time.Now().AddDate(100, 0, 0)
			}
			if aj.IsZero() {
				aj = time.Now().AddDate(100, 0, 0)
			}
			return ai.Before(aj)
		})
	default: // "recent"
		sort.SliceStable(accounts, func(i, j int) bool {
			ai, aj := accounts[i].LastInspected, accounts[j].LastInspected
			if ai.IsZero() {
				ai = time.Time{}
			}
			if aj.IsZero() {
				aj = time.Time{}
			}
			return ai.After(aj)
		})
	}
}
