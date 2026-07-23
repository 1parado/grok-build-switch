package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Repository       = "1parado/grok-build-switch"
	LatestReleaseAPI = "https://api.github.com/repos/" + Repository + "/releases/latest"
	CacheDuration    = 15 * time.Minute
)

type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type releaseResponse struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []Asset   `json:"assets"`
}

type Info struct {
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version"`
	UpdateAvailable bool      `json:"update_available"`
	Skipped         bool      `json:"skipped"`
	ReleaseName     string    `json:"release_name,omitempty"`
	ReleaseURL      string    `json:"release_url,omitempty"`
	DownloadURL     string    `json:"download_url,omitempty"`
	DownloadName    string    `json:"download_name,omitempty"`
	Notes           string    `json:"notes,omitempty"`
	PublishedAt     time.Time `json:"published_at,omitempty"`
}

type Checker struct {
	CurrentVersion string
	AssetName      string
	Endpoint       string
	Client         *http.Client

	mu          sync.Mutex
	lastChecked time.Time
	lastInfo    Info
}

func New(currentVersion, executableName string) *Checker {
	assetName := filepath.Base(executableName)
	if assetName == "" || !strings.HasSuffix(strings.ToLower(assetName), ".exe") {
		assetName = "grok_switch.exe"
	}
	return &Checker{
		CurrentVersion: strings.TrimSpace(currentVersion),
		AssetName:      assetName,
		Endpoint:       LatestReleaseAPI,
		Client:         &http.Client{Timeout: 8 * time.Second},
	}
}

func (c *Checker) Check(ctx context.Context) (Info, error) {
	if c == nil {
		return Info{}, fmt.Errorf("update checker is not configured")
	}
	c.mu.Lock()
	if !c.lastChecked.IsZero() && time.Since(c.lastChecked) < CacheDuration {
		info := c.lastInfo
		c.mu.Unlock()
		return info, nil
	}
	c.mu.Unlock()

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = LatestReleaseAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Info{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "grok-switch-update-checker")
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("GitHub Release 查询失败：HTTP %d", resp.StatusCode)
	}
	var release releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Info{}, fmt.Errorf("解析 GitHub Release 失败：%w", err)
	}
	if release.TagName == "" || release.Draft || release.Prerelease {
		return Info{}, fmt.Errorf("GitHub 没有可用的稳定 Release")
	}
	info := Info{
		CurrentVersion:  c.CurrentVersion,
		LatestVersion:   release.TagName,
		UpdateAvailable: IsNewer(release.TagName, c.CurrentVersion),
		ReleaseName:     release.Name,
		ReleaseURL:      release.HTMLURL,
		Notes:           release.Body,
		PublishedAt:     release.PublishedAt,
		DownloadName:    c.AssetName,
	}
	for _, asset := range release.Assets {
		if strings.EqualFold(asset.Name, c.AssetName) {
			info.DownloadURL = asset.DownloadURL
			break
		}
	}
	if info.DownloadURL == "" {
		info.DownloadURL = "https://github.com/" + Repository + "/releases/latest/download/" + c.AssetName
	}
	c.mu.Lock()
	c.lastChecked = time.Now()
	c.lastInfo = info
	c.mu.Unlock()
	return info, nil
}

func IsNewer(latest, current string) bool {
	latestParts, latestOK := versionParts(latest)
	currentParts, currentOK := versionParts(current)
	if !latestOK || !currentOK {
		return false
	}
	for len(latestParts) < len(currentParts) {
		latestParts = append(latestParts, 0)
	}
	for len(currentParts) < len(latestParts) {
		currentParts = append(currentParts, 0)
	}
	for i := range latestParts {
		if latestParts[i] != currentParts[i] {
			return latestParts[i] > currentParts[i]
		}
	}
	return false
}

func versionParts(value string) ([]int, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "v"))
	if value == "" || value == "dev" {
		return nil, false
	}
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		value = value[:dash]
	}
	parts := strings.Split(value, ".")
	result := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return nil, false
		}
		result[i] = number
	}
	return result, true
}
