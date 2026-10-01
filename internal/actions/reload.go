package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"tomcat-ops-helper/internal/config"
)

// ReloadData is the JSON returned by "reload_config".
type ReloadData struct {
	Reloaded        bool            `json:"reloaded"`
	ConfigLoadedAt  string          `json:"config_loaded_at"`
	ToolListPushed  bool            `json:"tool_list_pushed"`
	ToolListPushErr string          `json:"tool_list_push_err,omitempty"`
	Validation      json.RawMessage `json:"validation"`
}

// reloadConfig re-reads config.json from disk, validates it, and swaps it
// into the live helper without a process restart (todo G4② from the
// 2026-09-15 field session).
//
// Semantics:
//   - LoadHelper failure (parse / validate) aborts the reload with an
//     error and leaves the running config untouched — a typo in
//     config.json can never wedge the helper.
//   - On success the fresh config is copied over the shared *HelperConfig
//     pointer, so Registry, the execshell Executor, and the Hello sender
//     (client_id / token at the next reconnect) all pick it up. HelperConfig
//     carries no locks, making the struct copy vet-clean. In-flight
//     commands may observe a mid-swap mix of old/new fields; helper
//     dispatch is low-concurrency, so the window is negligible.
//   - dbReloader (injected by main) then closes and re-wires the DB
//     connectors; a query mid-flight on a closed connector errors, which
//     the caller re-issues.
//   - toolsChanged (injected by main) pushes a fresh ToolList so tool
//     availability updates on the server without a reconnect. A push
//     failure (e.g. momentarily disconnected) is reported in the reply,
//     not fatal: the list re-sends on the next reconnect anyway.
//
// What a reload does NOT cover: server_url / token / heartbeat intervals
// are captured by wsclient at startup, so changing those still requires a
// helper restart. The tool description states this.
func (r *Registry) reloadConfig(ctx context.Context) (json.RawMessage, error) {
	if r.configPath == "" {
		return nil, fmt.Errorf("reload_config: config path not wired (helper started without WithConfigPath); restart helper to pick up config changes")
	}
	fresh, err := config.LoadHelper(r.configPath)
	if err != nil {
		// Old config stays active; nothing has been swapped yet.
		return nil, fmt.Errorf("reload_config: %w (running config unchanged)", err)
	}

	r.reloadMu.Lock()
	*r.cfg = *fresh
	if r.dbReloader != nil {
		r.dbReloader()
	}
	r.reloadMu.Unlock()

	pushed := false
	pushErr := ""
	if r.toolsChanged != nil {
		pushed = true
		if err := r.toolsChanged(); err != nil {
			pushErr = err.Error()
		}
	}

	validation, err := r.validateConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("reload_config: config swapped but validation failed: %w", err)
	}
	return mustMarshal(ReloadData{
		Reloaded:        true,
		ConfigLoadedAt:  r.cfg.LoadedAt.UTC().Format(time.RFC3339),
		ToolListPushed:  pushed,
		ToolListPushErr: pushErr,
		Validation:      validation,
	}), nil
}
