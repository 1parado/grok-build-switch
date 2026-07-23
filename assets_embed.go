package main

import "embed"

// Shared by the tray/browser edition and the optional Wails GUI edition.
//
//go:embed ui/index.html ui/app.js ui/style.css ui/skill.svg ui/vendor icon.svg assets/icon.ico gui/index.html
var assets embed.FS
