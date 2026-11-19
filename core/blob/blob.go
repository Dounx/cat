// Package blob resolves and prunes attachment files kept under Cat's blobs directory.
package blob

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Resolve returns the clean, symlink-verified path inside blobsDir for a
// slash-separated relative blob path.
func Resolve(blobsDir, rel string) (string, error) {
	if blobsDir == "" || rel == "" || strings.ContainsRune(rel, '\x00') {
		return "", errors.New("invalid blob path")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid blob path")
	}
	baseReal, err := filepath.EvalSymlinks(blobsDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(baseReal, clean)
	fullReal, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	relOut, err := filepath.Rel(baseReal, fullReal)
	if err != nil || relOut == "." || relOut == ".." || strings.HasPrefix(relOut, ".."+string(filepath.Separator)) {
		return "", errors.New("blob path escapes root")
	}
	return fullReal, nil
}

// Sweep removes blob files older than grace that are not referenced by keepPaths,
// and cleans up empty subdirectories.
func Sweep(blobsDir string, keepPaths map[string]bool, grace time.Duration) {
	if blobsDir == "" {
		return
	}
	now := time.Now()
	var dirs []string
	_ = filepath.WalkDir(blobsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != blobsDir {
				dirs = append(dirs, path)
			}
			return nil
		}
		if keepPaths[path] {
			return nil
		}
		if real, err := filepath.EvalSymlinks(path); err == nil && keepPaths[real] {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if grace > 0 && now.Sub(info.ModTime()) < grace {
			return nil
		}
		_ = os.Remove(path)
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // succeeds only when empty
	}
}
