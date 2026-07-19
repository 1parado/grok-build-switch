//go:build wailsgui && windows

package main

import (
	"context"
	"testing"
)

func TestGUITrayCloseAndExplicitQuitLifecycle(t *testing.T) {
	controller := newGUITrayController("http://127.0.0.1:17878", nil)
	var shown, hidden, quit int
	controller.showAction = func(context.Context) { shown++ }
	controller.hideAction = func(context.Context) { hidden++ }
	controller.quitAction = func(context.Context) { quit++ }

	ctx := context.Background()
	controller.startup(ctx)
	if prevent := controller.beforeClose(ctx); prevent {
		t.Fatal("close was prevented before tray became ready")
	}
	controller.ready.Store(true)
	if prevent := controller.beforeClose(ctx); !prevent || hidden != 1 {
		t.Fatalf("ready close: prevent=%v hidden=%d, want true/1", prevent, hidden)
	}

	controller.showWindow()
	if shown != 1 {
		t.Fatalf("shown = %d, want 1", shown)
	}

	controller.requestQuit()
	controller.requestQuit()
	if quit != 1 {
		t.Fatalf("quit = %d, want exactly 1", quit)
	}
	if prevent := controller.beforeClose(ctx); prevent {
		t.Fatal("explicit quit was intercepted by close-to-tray")
	}
}

func TestGUITrayQuitRequestedBeforeWailsStartup(t *testing.T) {
	controller := newGUITrayController("http://127.0.0.1:17878", nil)
	quit := 0
	controller.quitAction = func(context.Context) { quit++ }
	controller.requestQuit()
	if quit != 0 {
		t.Fatalf("quit before startup = %d, want 0", quit)
	}
	controller.startup(context.Background())
	if quit != 1 {
		t.Fatalf("quit after startup = %d, want 1", quit)
	}
}
