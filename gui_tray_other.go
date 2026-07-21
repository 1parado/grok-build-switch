//go:build wailsgui && !windows

package main

import "context"

type guiTrayController struct{}

func newGUITrayController(url string, icon []byte) *guiTrayController {
	return &guiTrayController{}
}

func (t *guiTrayController) register()                            {}
func (t *guiTrayController) startup(ctx context.Context)          {}
func (t *guiTrayController) beforeClose(ctx context.Context) bool { return false }
func (t *guiTrayController) shutdown()                             {}
