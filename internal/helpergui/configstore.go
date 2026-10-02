package helpergui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tomcat-ops-helper/internal/config"
)

// restartKeys are config fields the helper captures at process start
// (wsclient.Options is built once); changing them means a restart, not
// just a reload_config.
var restartKeys = map[string]bool{
	"server_url":         true,
	"token":              true,
	"reconnect_interval": true,
	"heartbeat_interval": true,
	"heartbeat_timeout":  true,
}

// ConfigStore loads and atomically saves config.json as a raw document
// (map) so fields the GUI does not manage — `_comment_`, future keys,
// agent edits made while the page was open — always survive a save.
type ConfigStore struct {
	Path string
}

// LoadDoc returns the on-disk document verbatim. exists is false when
// the file is missing (callers substitute defaultDoc()).
func (cs *ConfigStore) LoadDoc() (doc map[string]any, exists bool, err error) {
	raw, err := os.ReadFile(cs.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	doc = map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, true, fmt.Errorf("config.json 不是合法 JSON: %w", err)
	}
	return doc, true, nil
}

// Save overlays fields onto the current on-disk document and writes it
// atomically: tmp file (same dir) → fsync → full LoadHelper validation
// on the tmp → rename over the real file. A validation error leaves the
// original untouched and no debris behind. A nil value deletes the key
// (this is how the UI removes the esb/erp blocks).
//
// restartRequired reports whether any wsclient-captured field changed
// (the UI pairs it with the running state to suggest a restart).
func (cs *ConfigStore) Save(fields map[string]any) (restartRequired bool, err error) {
	old, _, err := cs.LoadDoc()
	if err != nil {
		return false, err
	}
	if old == nil {
		old = map[string]any{}
	}

	restartRequired = false
	for k, v := range fields {
		was, had := old[k]
		if v == nil {
			delete(old, k)
		} else {
			old[k] = v
		}
		if restartKeys[k] && !sameJSONValue(was, old[k], had) {
			restartRequired = true
		}
	}

	b, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		return false, err
	}
	b = append(b, '\n')

	tmp := filepath.Join(filepath.Dir(cs.Path), filepath.Base(cs.Path)+fmt.Sprintf(".tmp-%d", os.Getpid()))
	if err := writeFileSync(tmp, b); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	// Validate the exact bytes that would land on disk, with the
	// project's own loader (flexible duration/size parsing + Validate).
	if _, err := config.LoadHelper(tmp); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, cs.Path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return restartRequired, nil
}

// defaultDoc mirrors rawHelper's defaults in config.go so a fresh
// install gets sensible starting values without touching disk.
func defaultDoc() map[string]any {
	return map[string]any{
		"server_url":        "wss://REPLACE_ME.trycloudflare.com/ws",
		"token":             "",
		"client_id":         "",
		"tomcat_home":       `C:\tomcat`,
		"jdk_home":          `C:\jdk-17`,
		"log_dir":           `C:\tomcat\logs`,
		"allowed_dirs":      []any{`D:\REPLACE_ME`},
		"command_timeout":   30,
		"max_output_size":   1048576,
		"reconnect_interval": 5,
		"heartbeat_interval": 30,
		"heartbeat_timeout": 90,
		"log_file":          "helper.log",
	}
}

// sameJSONValue compares two config values with JSON semantics so an
// unchanged field sent back by the UI (e.g. number vs float64 round
// trip) doesn't trip the restart hint.
func sameJSONValue(a, b any, had bool) bool {
	if !had {
		return b == nil
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func writeFileSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
