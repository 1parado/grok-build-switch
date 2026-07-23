package main

import (
	"context"
	"time"

	"grok_switch/internal/crash"
	"grok_switch/internal/notify"
	"grok_switch/internal/updatecheck"
)

const (
	initialUpdateCheckDelay = 15 * time.Second
	updateCheckInterval     = 24 * time.Hour
)

func startUpdateNotification(checker *updatecheck.Checker, state *updatecheck.PreferenceStore, title string) {
	if checker == nil || state == nil {
		return
	}
	go func() {
		check := func() {
			info, err := checker.Check(context.Background())
			if err != nil {
				crash.Logf("update check failed: %v", err)
				return
			}
			if !info.UpdateAvailable {
				return
			}
			claimed, err := state.ClaimNotification(info.LatestVersion)
			if err != nil {
				crash.Logf("record update notification failed: %v", err)
				return
			}
			if claimed {
				notify.Info(title, "发现新版本 "+info.LatestVersion+"，打开管理页面即可下载")
			}
		}
		timer := time.NewTimer(initialUpdateCheckDelay)
		defer timer.Stop()
		<-timer.C
		check()
		ticker := time.NewTicker(updateCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			check()
		}
	}()
}
