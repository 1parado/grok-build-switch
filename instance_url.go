package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"grok_switch/internal/settings"
)

func waitForExistingInstanceURL(store *settings.Store, dataDir string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	var lastErr error
	for {
		current, err := store.Get()
		if err != nil {
			lastErr = err
		} else {
			ports := []int{current.ActualPort}
			if current.Port != current.ActualPort {
				ports = append(ports, current.Port)
			}
			for _, port := range ports {
				if settings.ValidatePort(port) != nil {
					continue
				}
				url := fmt.Sprintf("http://127.0.0.1:%d", port)
				resp, requestErr := client.Get(url + "/api/status")
				if requestErr != nil {
					lastErr = requestErr
					continue
				}
				var status struct {
					DataDir string `json:"data_dir"`
				}
				decodeErr := json.NewDecoder(resp.Body).Decode(&status)
				_ = resp.Body.Close()
				if decodeErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && status.DataDir == dataDir {
					return url, nil
				}
				if decodeErr != nil {
					lastErr = decodeErr
				} else {
					lastErr = fmt.Errorf("port %d is not the running grok_switch instance", port)
				}
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("running instance did not become ready")
	}
	return "", lastErr
}
