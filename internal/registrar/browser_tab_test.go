package registrar

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestWaitFirstPageTargetIDPrefersBlankPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "ext-1", "type": "page", "url": "chrome-extension://abc/background.html"},
			{"id": "blank-1", "type": "page", "url": "about:blank"},
			{"id": "other-1", "type": "service_worker", "url": "chrome-extension://abc/sw.js"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	id, err := waitFirstPageTargetID(port, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if id != "blank-1" {
		t.Fatalf("got target %q, want blank-1", id)
	}
}
