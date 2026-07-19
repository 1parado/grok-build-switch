//go:build wailsgui && windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type guiTrayProvider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	IsActive bool   `json:"is_active"`
}

type guiTrayProviderSnapshot struct {
	Providers           []guiTrayProvider
	ActiveID            string
	ActiveName          string
	OfficialActive      bool
	ConfigMatchesActive bool
}

func (s guiTrayProviderSnapshot) currentName() string {
	if s.OfficialActive {
		return "官方账号"
	}
	if s.ActiveName != "" {
		return s.ActiveName
	}
	return "未设置"
}

func (s guiTrayProviderSnapshot) drifted() bool {
	return s.ActiveID != "" && !s.ConfigMatchesActive
}

func (s guiTrayProviderSnapshot) fingerprint() string {
	data, _ := json.Marshal(s)
	return string(data)
}

type guiTrayProviderClient struct {
	baseURL string
	client  *http.Client
}

func newGUITrayProviderClient(baseURL string) *guiTrayProviderClient {
	return &guiTrayProviderClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *guiTrayProviderClient) snapshot(ctx context.Context) (guiTrayProviderSnapshot, error) {
	var status struct {
		ActiveProfile struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"active_profile"`
		OfficialActive      bool `json:"official_active"`
		ConfigMatchesActive bool `json:"config_matches_active"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/status", &status); err != nil {
		return guiTrayProviderSnapshot{}, err
	}
	var providers []guiTrayProvider
	if err := c.do(ctx, http.MethodGet, "/api/profiles", &providers); err != nil {
		return guiTrayProviderSnapshot{}, err
	}
	return guiTrayProviderSnapshot{
		Providers:           providers,
		ActiveID:            status.ActiveProfile.ID,
		ActiveName:          status.ActiveProfile.Name,
		OfficialActive:      status.OfficialActive,
		ConfigMatchesActive: status.ConfigMatchesActive,
	}, nil
}

func (c *guiTrayProviderClient) activate(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/profiles/"+url.PathEscape(id)+"/activate", nil)
}

func (c *guiTrayProviderClient) activateOfficial(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/official/activate", nil)
}

func (c *guiTrayProviderClient) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var apiError struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiError)
		if apiError.Error != "" {
			return fmt.Errorf("%s", apiError.Error)
		}
		return fmt.Errorf("本地服务返回 HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析本地服务响应失败: %w", err)
	}
	return nil
}
