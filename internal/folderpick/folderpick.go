// Package folderpick opens a native directory chooser for local desktop use.
package folderpick

import (
	"context"
	"errors"
	"sync"
)

// ErrCancelled is returned when the user closes the dialog without a selection.
var ErrCancelled = errors.New("folder selection cancelled")

var pickMu sync.Mutex

// Directory shows a native folder dialog and returns the absolute path.
// start is an optional initial directory; empty uses the platform default.
// Concurrent calls are serialized so only one dialog is open at a time.
func Directory(ctx context.Context, start string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Serialize UI dialogs; hold the lock only for the platform call duration.
	pickMu.Lock()
	defer pickMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return pickDirectory(ctx, start)
}
