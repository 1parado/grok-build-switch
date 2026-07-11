package paths

import (
	"os"
	"path/filepath"
)

type Paths struct {
	GrokConfig   string
	GrokHome     string
	DataDir      string
	ProfilesFile string
	SettingsFile string
	BackupsDir   string
	LogFile       string
}

func Resolve() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	grokHome := os.Getenv("GROK_HOME")
	if grokHome == "" {
		grokHome = filepath.Join(home, ".grok")
	}
	grokConfig := os.Getenv("GROK_CONFIG")
	if grokConfig == "" {
		grokConfig = filepath.Join(grokHome, "config.toml")
	}
	dataDir := filepath.Join(home, ".grok_switch")
	return Paths{
		GrokConfig:   grokConfig,
		GrokHome:     grokHome,
		DataDir:      dataDir,
		ProfilesFile: filepath.Join(dataDir, "profiles.json"),
		SettingsFile: filepath.Join(dataDir, "settings.json"),
		BackupsDir:   filepath.Join(dataDir, "backups"),
		LogFile:      filepath.Join(dataDir, "grok_switch.log"),
	}, nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{p.DataDir, p.BackupsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}
