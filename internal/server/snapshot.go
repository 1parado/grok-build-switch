package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
)

// handleSnapshot 一次请求返回面板全量轮询数据（status、profiles、backups、
// settings、grokAuth、grokPool、registrar、lanAccess 的并集）。前端
// refreshAll 原先并发打 8 个端点：8 次连接调度、8 份 JSON 编解码、8 次鉴权
// 与中间件往返；合并后单请求单响应，轮询成本降为 1/8。
//
// 各字段与原独立端点的响应保持同构，前端无需改任何消费逻辑。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	response := map[string]any{}

	// status（与 handleStatus 同构）
	active, matches, err := s.Switcher.ActiveStatus()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	_, authErr := os.Stat(filepath.Join(s.Paths.GrokHome, "auth.json"))
	currentSettings, _ := s.Settings.Get()
	imagineAccounts := s.imagineAccountCount()
	response["status"] = map[string]any{
		"version":               s.Version,
		"active_profile":        active,
		"official_active":       active.ID == "",
		"official_logged_in":    authErr == nil,
		"config_path":           s.Paths.GrokConfig,
		"data_dir":              s.Paths.DataDir,
		"port":                  s.ActualPort,
		"settings":              currentSettings,
		"config_matches_active": matches,
		"imagine_accounts":      imagineAccounts,
		"imagine_ready":         imagineAccounts > 0,
	}

	// profiles / backups / settings
	if profiles, err := s.Profiles.List(); err == nil {
		response["profiles"] = profiles
	}
	if backups, err := s.Switcher.ListBackups(); err == nil {
		response["backups"] = backups
	}
	if settingsValue, err := s.Settings.Get(); err == nil {
		response["settings"] = settingsValue
	}

	// 账号与任务
	if s.GrokAuth != nil {
		if grokAuthResponse, err := s.grokAuthStatusResponse(); err == nil {
			response["grok_auth"] = grokAuthResponse
		}
	}
	if s.GrokPool != nil {
		response["grok_pool"] = s.GrokPool.Status()
	}
	if s.Registrar != nil {
		response["registrar"] = s.Registrar.Get()
	}
	if lanAccessSnapshot, err := s.lanAccessSnapshotResponse(); err == nil {
		response["lan_access"] = lanAccessSnapshot
	}

	writeJSON(w, response)
}
