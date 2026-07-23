package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"

	"grok_switch/internal/autostart"
	grokconfig "grok_switch/internal/config"
	"grok_switch/internal/cpamint"
	"grok_switch/internal/grokauth"
	"grok_switch/internal/grokpool"
	"grok_switch/internal/paths"
	"grok_switch/internal/profiles"
	"grok_switch/internal/registrar"
	"grok_switch/internal/remoteaccess"
	"grok_switch/internal/settings"
	"grok_switch/internal/switcher"
	"grok_switch/internal/updatecheck"
)

type Server struct {
	Paths         paths.Paths
	Profiles      *profiles.Store
	Settings      *settings.Store
	RemoteAccess  *remoteaccess.Store
	GrokAuth      *grokauth.Store
	GrokPool      *grokpool.Manager
	CpaMint       *cpamint.Service
	Registrar     *registrar.Service
	Switcher      *switcher.Switcher
	Agent         AgentService
	Assets        embed.FS
	ExePath       string
	Version       string
	UpdateChecker *updatecheck.Checker
	UpdateState   *updatecheck.PreferenceStore
	ActualPort    int
	onChanged     func()
	listenerMu    sync.Mutex
	listener      net.Listener
	bindHost      string
	httpServer    *http.Server
	loginMu       sync.Mutex
	loginFails    map[string]loginFailure
}

func (s *Server) SetOnChanged(fn func()) {
	s.onChanged = fn
}

func (s *Server) Listen(preferred int) (*http.Server, int, error) {
	if err := settings.ValidatePort(preferred); err != nil {
		return nil, 0, err
	}
	if s.GrokPool != nil {
		s.GrokPool.SetOnAuthDirImport(func(grokpool.ImportResult) {
			if _, err := s.upsertGrokAuthProfile(); err != nil {
				fmt.Fprintf(os.Stderr, "grok auth profile after hot-load: %v\n", err)
				return
			}
			s.changed()
		})
	}
	if s.Registrar != nil && s.GrokPool != nil {
		s.Registrar.SetAuthDirResolver(s.GrokPool.ResolvedAuthDir)
		s.Registrar.SetOnFinished(func(job registrar.Job) {
			if s.GrokPool == nil {
				return
			}
			result, err := s.GrokPool.ImportAuthDir()
			s.Registrar.SetImportResult(job.ID, result.Imported, result.Updated, err)
			if err == nil && result.Imported+result.Updated > 0 {
				if _, profileErr := s.upsertGrokAuthProfile(); profileErr == nil {
					s.changed()
				}
			}
		})
	}
	mux := http.NewServeMux()
	s.routes(mux)
	var listener net.Listener
	var err error
	port := preferred
	currentSettings := settings.Default()
	if s.Settings != nil {
		currentSettings, err = s.Settings.Get()
		if err != nil {
			return nil, 0, err
		}
	}
	bindHost := s.bindHostFor(currentSettings.LANAccessEnabled)
	for i := 0; i < 20 && port <= settings.MaxPort; i++ {
		listener, err = net.Listen("tcp", bindHost+":"+strconv.Itoa(port))
		if err == nil {
			break
		}
		port++
	}
	if listener == nil {
		listener, err = net.Listen("tcp", bindHost+":0")
		if err == nil {
			if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
				port = tcpAddr.Port
			}
		}
	}
	if listener == nil {
		return nil, 0, err
	}
	s.ActualPort = port
	if s.Switcher != nil {
		s.Switcher.ImagineProxyBaseURL = s.localImagineURL()
		if _, err := s.Switcher.ReapplyActive(); err != nil {
			// No active profile, or config missing — non-fatal at startup.
			fmt.Fprintf(os.Stderr, "imagine proxy reapply: %v\n", err)
		}
	}
	if err := s.ensureGrokAuthProfile(); err != nil {
		fmt.Fprintf(os.Stderr, "grok auth profile: %v\n", err)
	}
	if s.Settings != nil {
		if err := s.Settings.SetActualPort(port); err != nil {
			listener.Close()
			return nil, 0, err
		}
	}
	srv := &http.Server{
		Handler:           s.withAccess(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.listenerMu.Lock()
	s.listener = listener
	s.bindHost = bindHost
	s.httpServer = srv
	s.listenerMu.Unlock()
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(os.Stderr, "http server: %v\n", err)
		}
	}()
	return srv, port, nil
}

func (s *Server) reconfigureLANAccess(enabled bool) error {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	desired := s.bindHostFor(enabled)
	if s.listener == nil || s.bindHost == desired {
		return nil
	}
	oldListener := s.listener
	oldHost := s.bindHost
	_ = oldListener.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort(desired, strconv.Itoa(s.ActualPort)))
	if err != nil {
		restored, restoreErr := net.Listen("tcp", net.JoinHostPort(oldHost, strconv.Itoa(s.ActualPort)))
		if restoreErr == nil {
			s.listener = restored
			go s.serveListenerLocked(restored)
		}
		return err
	}
	s.listener = listener
	s.bindHost = desired
	go s.serveListenerLocked(listener)
	return nil
}

func (s *Server) serveListenerLocked(listener net.Listener) {
	if s.httpServer == nil {
		return
	}
	if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		fmt.Fprintf(os.Stderr, "http server reconfigure: %v\n", err)
	}
}

func (s *Server) Shutdown(ctx context.Context, srv *http.Server) error {
	return srv.Shutdown(ctx)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/pair", s.handlePair)
	mux.HandleFunc("/api/lan-access", s.handleLANAccess)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/update", s.handleUpdate)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/profiles/", s.handleProfileByID)
	mux.HandleFunc("/api/official/activate", s.handleOfficialActivate)
	mux.HandleFunc("/api/import", s.handleImport)
	mux.HandleFunc("/api/backups", s.handleBackups)
	mux.HandleFunc("/api/backups/", s.handleBackupByFile)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/models/fetch", s.handleFetchModels)
	mux.HandleFunc("/api/connection/test", s.handleConnectionTest)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/config/preview", s.handleConfigPreview)
	mux.HandleFunc("/api/config/privacy", s.handleConfigPrivacy)
	mux.HandleFunc("/api/grok-auth", s.handleGrokAuth)
	mux.HandleFunc("/api/grok-auth/refresh", s.handleGrokAuthRefresh)
	mux.HandleFunc("/api/grok-pool", s.handleGrokPool)
	mux.HandleFunc("/api/grok-pool/inspect", s.handleGrokPoolInspect)
	mux.HandleFunc("/api/grok-pool/bulk", s.handleGrokPoolBulk)
	mux.HandleFunc("/api/grok-pool/import-dir", s.handleGrokPoolImportDir)
	mux.HandleFunc("/api/grok-pool/open-auth-dir", s.handleGrokPoolOpenAuthDir)
	mux.HandleFunc("/api/grok-pool/accounts/", s.handleGrokPoolAccount)
	mux.HandleFunc("/api/cpa-mint", s.handleCpaMint)
	mux.HandleFunc("/api/registrar", s.handleRegistrar)
	mux.HandleFunc("/api/registrar/probe", s.handleRegistrarProbe)
	mux.HandleFunc("/api/registrar/start", s.handleRegistrarStart)
	mux.HandleFunc("/api/registrar/stop", s.handleRegistrarStop)
	mux.HandleFunc("/api/registrar/job", s.handleRegistrarJob)
	mux.HandleFunc("/api/registrar/job/log", s.handleRegistrarLog)
	mux.HandleFunc("/api/grok-config-models", s.handleGrokConfigModels)
	mux.HandleFunc("/api/skills", s.handleSkills)
	mux.HandleFunc("/api/skills/delete", s.handleSkillsDelete)
	mux.HandleFunc("/api/agent/status", s.handleAgentStatus)
	mux.HandleFunc("/api/agent/start", s.handleAgentStart)
	mux.HandleFunc("/api/agent/stop", s.handleAgentStop)
	mux.HandleFunc("/api/agent/cancel", s.handleAgentCancel)
	mux.HandleFunc("/api/agent/session", s.handleAgentSession)
	mux.HandleFunc("/api/agent/session/load", s.handleAgentSessionLoad)
	mux.HandleFunc("/api/agent/sessions", s.handleAgentSessions)
	mux.HandleFunc("/api/agent/sessions/", s.handleAgentSessionHistory)
	mux.HandleFunc("/api/agent/media", s.handleAgentMedia)
	mux.HandleFunc("/api/agent/session/rename", s.handleAgentRename)
	mux.HandleFunc("/api/agent/ws", s.handleAgentWebSocket)
	mux.HandleFunc("/grok/v1", s.handleGrokProxy)
	mux.HandleFunc("/grok/v1/", s.handleGrokProxy)
	mux.HandleFunc("/imagine/v1", s.handleImagineProxy)
	mux.HandleFunc("/imagine/v1/", s.handleImagineProxy)
	mux.HandleFunc("/", s.handleStatic)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.UpdateChecker == nil {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"current_version": "dev", "update_available": false})
			return
		}
		methodNotAllowed(w)
		return
	}
	info, err := s.UpdateChecker.Check(r.Context())
	if err != nil {
		writeJSONStatus(w, map[string]string{"error": err.Error()}, http.StatusBadGateway)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if s.UpdateState != nil {
			prefs, stateErr := s.UpdateState.Get()
			if stateErr != nil {
				writeJSONStatus(w, map[string]string{"error": stateErr.Error()}, http.StatusInternalServerError)
				return
			}
			info.Skipped = prefs.SkippedVersion == info.LatestVersion
		}
		writeJSON(w, info)
	case http.MethodPost:
		var req struct {
			Action  string `json:"action"`
			Version string `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if req.Action != "skip" || strings.TrimSpace(req.Version) == "" {
			writeError(w, fmt.Errorf("无效的更新操作"), http.StatusBadRequest)
			return
		}
		if req.Version != info.LatestVersion {
			writeError(w, fmt.Errorf("更新版本已变化，请重新检查"), http.StatusConflict)
			return
		}
		if s.UpdateState == nil {
			writeError(w, fmt.Errorf("更新状态未配置"), http.StatusInternalServerError)
			return
		}
		if err := s.UpdateState.Skip(req.Version); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		info.Skipped = true
		writeJSON(w, info)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) bindHostFor(enabled bool) string {
	if enabled {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	active, matches, err := s.Switcher.ActiveStatus()
	if err != nil && !os.IsNotExist(err) {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	currentSettings, _ := s.Settings.Get()
	_, authErr := os.Stat(filepath.Join(s.Paths.GrokHome, "auth.json"))
	writeJSON(w, map[string]any{
		"version":               s.Version,
		"active_profile":        active,
		"official_active":       active.ID == "",
		"official_logged_in":    authErr == nil,
		"config_path":           s.Paths.GrokConfig,
		"data_dir":              s.Paths.DataDir,
		"port":                  s.ActualPort,
		"settings":              currentSettings,
		"config_matches_active": matches,
	})
}

func (s *Server) handleOfficialActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.Switcher.ActivateOfficial(); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	authFile := filepath.Join(s.Paths.GrokHome, "auth.json")
	loginRequired := false
	if _, err := os.Stat(authFile); os.IsNotExist(err) {
		loginRequired = true
		if err := exec.Command("grok", "login").Start(); err != nil {
			writeError(w, fmt.Errorf("已切换到官方配置，但启动 grok login 失败: %w", err), http.StatusInternalServerError)
			return
		}
	}
	s.changed()
	writeJSON(w, map[string]any{
		"ok":             true,
		"login_required": loginRequired,
		"message":        "已切换到官方账号，新开 grok 会话生效",
	})
}

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.Profiles.List()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
	case http.MethodPost:
		var profile profiles.Profile
		if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		created, err := s.Profiles.Create(profile)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.changed()
		writeJSONStatus(w, created, http.StatusCreated)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleProfileByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, fmt.Errorf("missing profile id"), http.StatusBadRequest)
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "activate" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		profile, err := s.Switcher.Activate(id)
		if err != nil {
			status := http.StatusInternalServerError
			if os.IsNotExist(err) {
				status = http.StatusNotFound
			}
			writeError(w, err, status)
			return
		}
		s.changed()
		writeJSON(w, map[string]any{
			"profile": profile,
			"message": "已切换，新开 grok 会话生效",
		})
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var profile profiles.Profile
		if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		updated, err := s.Profiles.Update(id, profile)
		if err != nil {
			status := http.StatusInternalServerError
			if os.IsNotExist(err) {
				status = http.StatusNotFound
			}
			writeError(w, err, status)
			return
		}
		s.changed()
		writeJSON(w, updated)
	case http.MethodDelete:
		if err := s.Profiles.Delete(id); err != nil {
			status := http.StatusInternalServerError
			if os.IsNotExist(err) {
				status = http.StatusNotFound
			}
			writeError(w, err, status)
			return
		}
		s.changed()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Name == "" {
		req.Name = "Imported"
	}
	profile, err := s.Switcher.ImportCurrent(req.Name, req.Active)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	s.changed()
	writeJSONStatus(w, profile, http.StatusCreated)
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	backups, err := s.Switcher.ListBackups()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, backups)
}

func (s *Server) handleBackupByFile(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/backups/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "restore" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.Switcher.RestoreBackup(parts[0]); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	s.changed()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		current, err := s.Settings.Get()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, current)
	case http.MethodPut:
		current, currentErr := s.Settings.Get()
		if currentErr != nil {
			writeError(w, currentErr, http.StatusInternalServerError)
			return
		}
		var next settings.Settings
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if !isLoopbackRequest(r) {
			next.LANAccessEnabled = current.LANAccessEnabled
		}
		if current.LANAccessEnabled && !next.LANAccessEnabled && s.RemoteAccess != nil {
			if err := s.RemoteAccess.ResetSessions(); err != nil {
				writeError(w, fmt.Errorf("撤销局域网会话失败: %w", err), http.StatusInternalServerError)
				return
			}
		}
		updated, err := s.Settings.Update(next)
		if err != nil {
			status := http.StatusInternalServerError
			if settings.IsValidationError(err) {
				status = http.StatusBadRequest
			}
			writeError(w, err, status)
			return
		}
		if err := s.reconfigureLANAccess(updated.LANAccessEnabled); err != nil {
			current.LANAccessEnabled = !updated.LANAccessEnabled
			_, _ = s.Settings.Update(current)
			writeError(w, fmt.Errorf("切换局域网监听失败: %w", err), http.StatusInternalServerError)
			return
		}
		if err := autostart.Sync(updated.Autostart, s.ExePath, updated.SilentAutostart); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.changed()
		writeJSON(w, updated)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ProfileID      string `json:"profile_id"`
		BaseURL        string `json:"base_url"`
		APIKey         string `json:"api_key"`
		UpstreamFormat string `json:"upstream_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if req.ProfileID != "" {
		profile, err := s.Profiles.Get(req.ProfileID)
		if err == nil {
			if req.BaseURL == "" {
				req.BaseURL = profile.BaseURL
			}
			if req.APIKey == "" {
				req.APIKey = profile.EffectiveAPIKey()
			}
			if req.UpstreamFormat == "" {
				req.UpstreamFormat = profile.UpstreamFormat
			}
		}
	}
	models, err := fetchModelList(r.Context(), req.BaseURL, req.APIKey, req.UpstreamFormat)
	if err != nil {
		writeError(w, err, http.StatusBadGateway)
		return
	}
	if req.ProfileID != "" {
		if profile, err := s.Profiles.Get(req.ProfileID); err == nil {
			profile.AvailableModels = models
			profile.APIKey = req.APIKey
			profile.UpstreamFormat = req.UpstreamFormat
			_, _ = s.Profiles.Update(req.ProfileID, profile)
			s.changed()
		}
	}
	writeJSON(w, map[string]any{"models": models})
}

func (s *Server) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ProfileID      string `json:"profile_id"`
		BaseURL        string `json:"base_url"`
		APIKey         string `json:"api_key"`
		UpstreamFormat string `json:"upstream_format"`
		Model          string `json:"model"`
		APIBackend     string `json:"api_backend"`
		Purpose        string `json:"purpose"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if req.ProfileID != "" {
		if profile, err := s.Profiles.Get(req.ProfileID); err == nil {
			if req.BaseURL == "" {
				req.BaseURL = profile.BaseURL
			}
			if req.APIKey == "" {
				req.APIKey = profile.EffectiveAPIKey()
			}
			if req.UpstreamFormat == "" {
				req.UpstreamFormat = profile.UpstreamFormat
			}
		}
	}
	start := time.Now()
	// Per-model probe: send a minimal completion request.
	if strings.TrimSpace(req.Model) != "" {
		var err error
		if req.Purpose == "image_generation" {
			err = probeImageGeneration(r.Context(), req.BaseURL, req.APIKey, req.Model)
		} else {
			err = probeModel(r.Context(), req.BaseURL, req.APIKey, req.UpstreamFormat, req.APIBackend, req.Model)
		}
		latency := time.Since(start).Milliseconds()
		if err != nil {
			writeJSONStatus(w, map[string]any{
				"ok":         false,
				"latency_ms": latency,
				"error":      err.Error(),
				"model":      req.Model,
			}, http.StatusOK)
			return
		}
		writeJSON(w, map[string]any{
			"ok":         true,
			"latency_ms": latency,
			"model":      req.Model,
		})
		return
	}
	models, err := fetchModelList(r.Context(), req.BaseURL, req.APIKey, req.UpstreamFormat)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSONStatus(w, map[string]any{
			"ok":            false,
			"latency_ms":    latency,
			"error":         err.Error(),
			"model_count":   0,
			"sample_models": []string{},
		}, http.StatusOK)
		return
	}
	sample := models
	if len(sample) > 5 {
		sample = sample[:5]
	}
	writeJSON(w, map[string]any{
		"ok":            true,
		"latency_ms":    latency,
		"model_count":   len(models),
		"sample_models": sample,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := s.Switcher.ReadConfig()
		if err != nil && !os.IsNotExist(err) {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		exists := err == nil
		if os.IsNotExist(err) {
			data = []byte{}
		}
		writeJSON(w, map[string]any{
			"path":    s.Paths.GrokConfig,
			"content": string(data),
			"exists":  exists,
		})
	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Content) != "" {
			var probe map[string]any
			if err := toml.Unmarshal([]byte(req.Content), &probe); err != nil {
				writeError(w, fmt.Errorf("TOML 无效: %w", err), http.StatusBadRequest)
				return
			}
		}
		if err := s.Switcher.WriteConfig([]byte(req.Content)); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.changed()
		writeJSON(w, map[string]any{"ok": true, "path": s.Paths.GrokConfig})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleConfigPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var profile profiles.Profile
	if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	profile = profiles.Normalize(profile)
	opts := grokconfig.ApplyOpts{}
	if s.Switcher != nil {
		opts.ImagineProxyBaseURL = s.Switcher.ImagineProxyBaseURL
	}
	snippet, err := grokconfig.SnippetForProfile(profile, opts)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	full, err := grokconfig.PreviewApply(s.Paths.GrokConfig, profile, opts)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	note := "磁盘上只有一份生效的 config.toml。每个供应商的 URL/Key/模型保存在 grok_switch 的 profile 里；点「启用」或「保存并启用」时，才会把该供应商的字段写入这份文件。"
	if profile.ImageGeneration != nil && profile.ImageGeneration.Enabled && strings.TrimSpace(opts.ImagineProxyBaseURL) != "" {
		note += " 启用生图时，xai_api_base_url 会写成本地 /imagine/v1 代理，由 grok_switch 注入独立生图 API Key（Grok ImageGen 本身会带聊天 Key）。"
	}
	writeJSON(w, map[string]any{
		"path":    s.Paths.GrokConfig,
		"snippet": snippet,
		"full":    string(full),
		"note":    note,
	})
}

func (s *Server) handleConfigPrivacy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.Switcher.ApplyPrivacyProtection(); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	s.changed()
	writeJSON(w, map[string]any{
		"ok":      true,
		"path":    s.Paths.GrokConfig,
		"message": "隐私保护配置已写入 config.toml",
	})
}

func fetchModelList(ctx context.Context, baseURL, apiKey, upstreamFormat string) ([]string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("base_url is required")
	}
	client := &http.Client{Timeout: 12 * time.Second}
	var failures []string
	for _, endpoint := range modelEndpoints(baseURL) {
		models, err := fetchModelEndpoint(ctx, client, endpoint, apiKey)
		if err == nil && len(models) > 0 {
			return models, nil
		}
		if err != nil {
			failures = append(failures, endpoint+": "+err.Error())
		} else {
			failures = append(failures, endpoint+": empty model list")
		}
	}
	return nil, fmt.Errorf("failed to fetch %s model list: %s", upstreamFormat, strings.Join(failures, "; "))
}

func probeModel(ctx context.Context, baseURL, apiKey, upstreamFormat, apiBackend, model string) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	model = strings.TrimSpace(model)
	if baseURL == "" {
		return fmt.Errorf("base_url is required")
	}
	if model == "" {
		return fmt.Errorf("model is required")
	}
	backend := apiBackend
	if backend == "" {
		backend = profiles.APIBackendForUpstreamFormat(upstreamFormat)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var endpoint string
	var body map[string]any
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
	switch backend {
	case "messages":
		endpoint = baseURL + "/messages"
		// Some gateways expect /v1/messages already in base; also try raw.
		if !strings.HasSuffix(baseURL, "/v1") && !strings.Contains(baseURL, "/messages") {
			// keep as-is; many anthropic-compat proxies use /v1 base already
		}
		if strings.HasSuffix(baseURL, "/v1") {
			endpoint = baseURL + "/messages"
		}
		body = map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
		if apiKey != "" {
			headers["x-api-key"] = apiKey
			headers["Authorization"] = "Bearer " + apiKey
			headers["anthropic-version"] = "2023-06-01"
		}
	case "responses":
		endpoint = baseURL + "/responses"
		body = map[string]any{
			"model":             model,
			"input":             "ping",
			"max_output_tokens": 1,
		}
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
		}
	default:
		endpoint = baseURL + "/chat/completions"
		body = map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
		if apiKey != "" {
			headers["Authorization"] = "Bearer " + apiKey
			headers["x-api-key"] = apiKey
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s: %s", resp.Status, msg)
}

func probeImageGeneration(ctx context.Context, baseURL, apiKey, model string) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	model = strings.TrimSpace(model)
	if baseURL == "" {
		return fmt.Errorf("base_url is required")
	}
	if apiKey == "" {
		return fmt.Errorf("api_key is required")
	}
	if model == "" {
		return fmt.Errorf("model is required")
	}
	payload, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": "A simple blue circle centered on a white background",
		"n":      1,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/images/generations", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	message := strings.TrimSpace(string(raw))
	if message == "" {
		message = resp.Status
	}
	return fmt.Errorf("%s: %s", resp.Status, message)
}

func modelEndpoints(baseURL string) []string {
	candidates := []string{baseURL + "/models"}
	withoutV1 := strings.TrimSuffix(baseURL, "/v1")
	if withoutV1 != baseURL {
		candidates = append(candidates, withoutV1+"/v1/models")
	}
	return uniqueStrings(candidates)
}

func fetchModelEndpoint(ctx context.Context, client *http.Client, endpoint, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("returned %s", resp.Status)
	}
	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	models := extractModels(payload)
	if len(models) == 0 {
		return nil, fmt.Errorf("no models in response")
	}
	return models, nil
}

func extractModels(payload any) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id, ok := x["id"].(string); ok && id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
			if name, ok := x["name"].(string); ok && name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			for key, child := range x {
				if key == "data" || key == "models" {
					walk(child)
				}
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		case string:
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	walk(payload)
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "." || name == "" {
		name = "ui/index.html"
	} else if name == "icon.svg" {
		name = "icon.svg"
	} else {
		name = "ui/" + name
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Always revalidate UI assets so drift fixes and UI changes apply without hard cache.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	data, err := fs.ReadFile(s.Assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Write(data)
}

func (s *Server) changed() {
	if s.onChanged != nil {
		s.onChanged()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, v, http.StatusOK)
}

func writeJSONStatus(w http.ResponseWriter, v any, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error, status int) {
	writeJSONStatus(w, map[string]string{"error": err.Error()}, status)
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
}
