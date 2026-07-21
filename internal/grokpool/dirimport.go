package grokpool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImportDirectory reads JSON auth files from dir and imports them into the pool.
// Original files are never modified or moved.
func (m *Manager) ImportDirectory(dir string, recursive bool) (ImportResult, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ImportResult{}, fmt.Errorf("目录路径不能为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ImportResult{}, fmt.Errorf("解析目录路径: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return ImportResult{}, fmt.Errorf("读取目录: %w", err)
	}
	if !info.IsDir() {
		return ImportResult{}, fmt.Errorf("路径不是目录: %s", abs)
	}
	pathFiles, hashes, err := collectAuthJSONByPath(abs, recursive)
	if err != nil {
		return ImportResult{}, err
	}
	if len(pathFiles) == 0 {
		return ImportResult{}, fmt.Errorf("目录中没有可导入的 .json 认证文件: %s", abs)
	}
	files := make([]ImportFile, 0, len(pathFiles))
	for _, file := range pathFiles {
		files = append(files, file)
	}
	result, err := m.Import(files)
	if err != nil {
		return result, err
	}
	m.mu.Lock()
	if m.watchHashes == nil {
		m.watchHashes = make(map[string]string)
	}
	for path, hash := range hashes {
		m.watchHashes[path] = hash
	}
	m.watchFileCount = len(hashes)
	m.watchLastScan = time.Now().UTC()
	if result.Imported+result.Updated > 0 {
		m.watchLastImport = m.watchLastScan
		m.watchLastError = ""
	}
	m.mu.Unlock()
	return result, nil
}

// ImportAuthDir imports from the configured (or default) CPA auth directory.
func (m *Manager) ImportAuthDir() (ImportResult, error) {
	m.mu.Lock()
	dir := m.resolvedAuthDirLocked()
	recursive := m.state.Settings.WatchRecursive
	m.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ImportResult{}, err
	}
	return m.ImportDirectory(dir, recursive)
}

func (m *Manager) watchLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.scanAuthDirOnce()
		}
	}
}

func (m *Manager) scanAuthDirOnce() {
	m.mu.Lock()
	enabled := m.state.Settings.WatchEnabled
	dir := m.resolvedAuthDirLocked()
	recursive := m.state.Settings.WatchRecursive
	m.mu.Unlock()
	if !enabled {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.mu.Lock()
		m.watchLastError = err.Error()
		m.watchLastScan = time.Now().UTC()
		m.mu.Unlock()
		return
	}

	pathFiles, pathHashes, err := collectAuthJSONByPath(dir, recursive)
	m.mu.Lock()
	m.watchLastScan = time.Now().UTC()
	if err != nil {
		m.watchLastError = err.Error()
		m.mu.Unlock()
		return
	}
	if m.watchHashes == nil {
		m.watchHashes = make(map[string]string)
	}
	changed := make([]ImportFile, 0)
	for path, hash := range pathHashes {
		prev, ok := m.watchHashes[path]
		if !ok || prev != hash {
			if file, found := pathFiles[path]; found {
				changed = append(changed, file)
			}
			m.watchHashes[path] = hash
		}
	}
	for path := range m.watchHashes {
		if _, ok := pathHashes[path]; !ok {
			delete(m.watchHashes, path)
		}
	}
	m.watchFileCount = len(pathHashes)
	callback := m.onAuthDirImport
	m.mu.Unlock()

	if len(changed) == 0 {
		m.mu.Lock()
		m.watchLastError = ""
		m.mu.Unlock()
		return
	}
	result, importErr := m.Import(changed)
	m.mu.Lock()
	if importErr != nil {
		m.watchLastError = importErr.Error()
	} else {
		m.watchLastImport = time.Now().UTC()
		if len(result.Failed) > 0 {
			m.watchLastError = strings.Join(result.Failed, "; ")
		} else {
			m.watchLastError = ""
		}
	}
	m.mu.Unlock()
	if importErr == nil && callback != nil && (result.Imported+result.Updated > 0) {
		callback(result)
	}
}

func collectAuthJSONByPath(root string, recursive bool) (map[string]ImportFile, map[string]string, error) {
	files := make(map[string]ImportFile)
	hashes := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if !recursive {
				return fs.SkipDir
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".json") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) == 0 || len(data) > 1<<20 {
			return nil
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		rel, relErr := filepath.Rel(root, path)
		name := d.Name()
		if relErr == nil {
			name = rel
		}
		files[path] = ImportFile{Name: name, Content: string(data)}
		hashes[path] = hash
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return files, hashes, nil
}
