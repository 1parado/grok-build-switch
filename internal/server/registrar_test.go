package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"grok_switch/internal/registrar"
)

func TestRegistrarConfigAPI(t *testing.T) {
	service, err := registrar.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Registrar: service}

	get := httptest.NewRequest(http.MethodGet, "/api/registrar", nil)
	response := httptest.NewRecorder()
	server.handleRegistrar(response, get)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"email_provider":"cloudflare"`) {
		t.Fatalf("GET body = %s", response.Body.String())
	}

	body := `{
		"version":1,
		"browser_mode":"auto",
		"email_provider":"cloudmail",
		"hotmail_max_aliases":5,
		"count":2,
		"workers":1,
		"mail_timeout_seconds":180,
		"page_timeout_seconds":300,
		"prefer_protocol_mint":true
	}`
	put := httptest.NewRequest(http.MethodPut, "/api/registrar", strings.NewReader(body))
	response = httptest.NewRecorder()
	server.handleRegistrar(response, put)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := service.Get().Config.Count; got != 2 {
		t.Fatalf("saved count = %d", got)
	}
}

func TestRegistrarStartRejectsIncompleteConfig(t *testing.T) {
	service, err := registrar.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Registrar: service}
	request := httptest.NewRequest(http.MethodPost, "/api/registrar/start", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	server.handleRegistrarStart(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
}
