package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"grok_switch/internal/registrar"
)

func (s *Server) handleRegistrar(w http.ResponseWriter, r *http.Request) {
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.Registrar.Get())
	case http.MethodPut:
		var config registrar.Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&config); err != nil {
			writeError(w, fmt.Errorf("读取注册机配置: %w", err), http.StatusBadRequest)
			return
		}
		state, err := s.Registrar.Update(config)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, state)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleRegistrarProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	var config registrar.Config
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&config)
	}
	if config.Version == 0 {
		state := s.Registrar.Get()
		config = state.Config
	}
	writeJSON(w, s.Registrar.Probe(&config))
}

func (s *Server) handleRegistrarStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	job, err := s.Registrar.Start()
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSONStatus(w, map[string]any{"job": job}, http.StatusAccepted)
}

func (s *Server) handleRegistrarStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	job, err := s.Registrar.Stop()
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"job": job})
}

func (s *Server) handleRegistrarJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"job": s.Registrar.Job()})
}

func (s *Server) handleRegistrarLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块未初始化"), http.StatusServiceUnavailable)
		return
	}
	data, err := s.Registrar.ReadLog()
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(strings.TrimSpace(string(data)) + "\n"))
}
