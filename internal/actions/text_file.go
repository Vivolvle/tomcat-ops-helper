package actions

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// Serialize read/replace operations so two replacements do not lose updates.
var textFileMu sync.Mutex

const textFileLimit = 1024 * 1024

func (r *Registry) textFile(params json.RawMessage, replace bool) (json.RawMessage, error) {
	textFileMu.Lock()
	defer textFileMu.Unlock()
	var p struct {
		Path          string  `json:"path"`
		Old           string  `json:"old_text"`
		New           *string `json:"new_text"`
		ExpectedCount *int    `json:"expected_count"`
		SHA256        string  `json:"expected_sha256"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	path, err := filepath.Abs(p.Path)
	if err != nil {
		return nil, err
	}
	if !r.cfg.IsAllowedWorkDir(path) {
		return nil, fmt.Errorf("path not in allowed_dirs")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if !r.cfg.IsAllowedWorkDir(path) {
		return nil, fmt.Errorf("resolved path not in allowed_dirs")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("path must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, textFileLimit+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > textFileLimit {
		return nil, fmt.Errorf("text file exceeds 1 MiB limit; use tail for logs")
	}
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("file is not UTF-8; convert encoding before editing")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(b))
	if !replace {
		return mustMarshal(map[string]any{"path": path, "text": string(b), "bytes": len(b), "sha256": hash}), nil
	}
	if p.Old == "" || p.New == nil {
		return nil, fmt.Errorf("nonempty old_text and new_text are required")
	}
	if p.SHA256 != "" && !strings.EqualFold(hash, p.SHA256) {
		return nil, fmt.Errorf("file changed: expected_sha256 mismatch")
	}
	count := strings.Count(string(b), p.Old)
	expected := 1
	if p.ExpectedCount != nil {
		expected = *p.ExpectedCount
	}
	if expected < 1 || count != expected {
		return nil, fmt.Errorf("matched %d occurrences; expected_count=%d; file unchanged", count, expected)
	}
	updated := []byte(strings.ReplaceAll(string(b), p.Old, *p.New))
	if len(updated) > textFileLimit {
		return nil, fmt.Errorf("replacement exceeds 1 MiB limit")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".replace-*")
	if err != nil {
		return nil, err
	}
	tempPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tempPath)
	if err := writeAtomic(tempPath, path, updated, info.Mode().Perm()); err != nil {
		return nil, err
	}
	return mustMarshal(map[string]any{"path": path, "replacements": count, "bytes": len(updated), "sha256": fmt.Sprintf("%x", sha256.Sum256(updated))}), nil
}
