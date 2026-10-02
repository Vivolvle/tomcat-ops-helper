package helpergui

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// DirEntryInfo is one browsable directory in the folder picker.
type DirEntryInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ListDirs returns the subdirectories of path (directories only —
// never files), sorted by name, plus the parent path. parent is empty
// at a filesystem root (drive letter / volume), which the UI renders as
// "回到驱动器列表". Symlinks are skipped to avoid cycles; unreadable
// entries are silently dropped; the list is capped at maxDirEntries.
func ListDirs(path string) (parent string, entries []DirEntryInfo, err error) {
	if path == "" {
		return "", nil, fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	dirents, err := os.ReadDir(abs)
	if err != nil {
		return "", nil, err
	}
	entries = make([]DirEntryInfo, 0, len(dirents))
	for _, e := range dirents {
		if !e.IsDir() {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if len(entries) >= maxDirEntries {
			break
		}
		full := filepath.Join(abs, e.Name())
		entries = append(entries, DirEntryInfo{Name: e.Name(), Path: full})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	parent = filepath.Dir(abs)
	if parent == abs { // filesystem root: drive letter or volume
		parent = ""
	}
	return parent, entries, nil
}

const maxDirEntries = 2000
