package grokpool

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"grok_switch/internal/grokauth"
)

const (
	defaultIntervalMinutes = 360
	defaultWorkers         = 4
	minIntervalMinutes     = 30
	maxIntervalMinutes     = 1440
	minWorkers             = 1
	maxWorkers             = 16
	poolVersion            = 1
)

type Settings struct {
	Enabled         bool   `json:"enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	Workers         int    `json:"workers"`
	ProxyURL        string `json:"proxy_url,omitempty"`
	// AuthDir is the CPA xai-*.json directory used for path import and hot-load.
	// Empty means default: <dataDir>/cpa_auths next to the pool directory.
	AuthDir        string `json:"auth_dir,omitempty"`
	WatchEnabled   bool   `json:"watch_enabled"`
	WatchRecursive bool   `json:"watch_recursive"`
}

type Account struct {
	ID             string    `json:"id"`
	FileName       string    `json:"file_name,omitempty"`
	Email          string    `json:"email,omitempty"`
	Source         string    `json:"source,omitempty"`
	Disabled       bool      `json:"disabled"`
	Classification string    `json:"classification"`
	Reason         string    `json:"reason,omitempty"`
	Action         string    `json:"action"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	Model          string    `json:"model,omitempty"`
	ErrorCode      string    `json:"error_code,omitempty"`
	ErrorMessage   string    `json:"error_message,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty,omitzero"`
	ImportedAt     time.Time `json:"imported_at"`
	LastInspected  time.Time `json:"last_inspected,omitempty,omitzero"`
}

type Status struct {
	Configured  bool      `json:"configured"`
	LocalAPIKey string    `json:"local_api_key,omitempty"`
	Settings    Settings  `json:"settings"`
	Accounts    []Account `json:"accounts"`
	Summary     Summary   `json:"summary"`
	Running     bool      `json:"running"`
	Done        int       `json:"done"`
	Total       int       `json:"total"`
	LastRun     time.Time `json:"last_run,omitempty,omitzero"`
	NextRun     time.Time `json:"next_run,omitempty,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	// ResolvedAuthDir is the absolute CPA auth directory currently in use.
	ResolvedAuthDir string    `json:"resolved_auth_dir,omitempty"`
	WatchLastScan   time.Time `json:"watch_last_scan,omitempty,omitzero"`
	WatchLastImport time.Time `json:"watch_last_import,omitempty,omitzero"`
	WatchLastError  string    `json:"watch_last_error,omitempty"`
	WatchFileCount  int       `json:"watch_file_count,omitempty"`
}

type Summary struct {
	Total       int `json:"total"`
	Available   int `json:"available"`
	Healthy     int `json:"healthy"`
	Abnormal    int `json:"abnormal"`
	Uninspected int `json:"uninspected"`
	Permission  int `json:"permission_denied"`
	Quota       int `json:"quota_exhausted"`
	Reauth      int `json:"reauth"`
	ProbeError  int `json:"probe_error"`
	Disabled    int `json:"disabled"`
}

type ImportFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ImportResult struct {
	Imported int      `json:"imported"`
	Updated  int      `json:"updated"`
	Failed   []string `json:"failed,omitempty"`
}

type BulkResult struct {
	Action  string   `json:"action"`
	Matched int      `json:"matched"`
	Updated int      `json:"updated"`
	Failed  []string `json:"failed,omitempty"`
}

type persistedState struct {
	Version     int       `json:"version"`
	LocalAPIKey string    `json:"local_api_key"`
	Settings    Settings  `json:"settings"`
	Accounts    []Account `json:"accounts"`
	LastRun     time.Time `json:"last_run,omitempty,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
}

type Manager struct {
	dir         string
	indexPath   string
	accountsDir string
	client      *http.Client
	transport   *http.Transport
	upstreamURL string

	mu             sync.Mutex
	state          persistedState
	running        bool
	rerunRequested bool
	doneCount      int
	totalCount     int
	nextRun        time.Time
	started        bool
	wake           chan struct{}
	stop           chan struct{}
	done           chan struct{}
	runCancel      context.CancelFunc
	runWG          sync.WaitGroup
	roundRobin     atomic.Uint64
	stores         map[string]*grokauth.Store

	// Auth-dir hot-load state (in-memory fingerprints; re-scanned after restart).
	watchHashes     map[string]string
	watchLastScan   time.Time
	watchLastImport time.Time
	watchLastError  string
	watchFileCount  int
	onAuthDirImport func(ImportResult)
}

func defaultSettings() Settings {
	return Settings{
		Enabled:         true,
		IntervalMinutes: defaultIntervalMinutes,
		Workers:         defaultWorkers,
		WatchRecursive:  true,
	}
}
