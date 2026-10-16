package actions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"tomcat-ops-helper/internal/execshell"
)

func TestUploadedJarSameDestination(t *testing.T) {
	cfg := mustCfg(t)
	r := NewRegistry(cfg, execshell.NewExecutor(cfg))
	path := filepath.Join(cfg.TomcatHome, "uploaded.jar")
	if err := os.WriteFile(path, []byte("uploaded jar"), 0600); err != nil {
		t.Fatal(err)
	}
	p := mustMarshal(map[string]any{"src": path, "dest": path, "restart": false})
	for _, tool := range []string{"replace_jar", "deploy"} {
		if _, err := r.Dispatch(context.Background(), tool, p); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "uploaded jar" {
			t.Fatalf("file changed: %q %v", b, err)
		}
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatal("same-path deployment created backup")
	}
}

func TestUploadGuideBridgeURL(t *testing.T) {
	cfg := mustCfg(t)
	r := NewRegistry(cfg, execshell.NewExecutor(cfg))
	for _, relay := range []string{"wss://tunnel.example/ws", "wss://example:8443", "ws://[::1]:8444"} {
		cfg.ServerURL = relay
		raw, err := r.uploadGuide(mustMarshal(map[string]any{"source": "app.jar", "destination": "remote.jar"}))
		if err != nil {
			t.Fatal(err)
		}
		var d UploadGuideData
		json.Unmarshal(raw, &d)
		if d.ServerURL != "http://127.0.0.1:7777" {
			t.Fatal(d.ServerURL)
		}
		if !strings.Contains(strings.Join(d.Steps, "\n"), `-F "sha256=$sha"`) {
			t.Fatal("hash does not expand")
		}
	}
	_, err := r.uploadGuide(mustMarshal(map[string]any{"source": "app.jar", "destination": "remote.jar", "server_url": "http:7777"}))
	if err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestRestartAsyncSurvivesCallerCancellation(t *testing.T) {
	cfg := mustCfg(t)
	r := NewRegistry(cfg, execshell.NewExecutor(cfg).WithRunner(&captureRunner{})).WithProbe(&fakeProbe{})
	ctx, cancel := context.WithCancel(context.Background())
	raw, err := r.restart(ctx, json.RawMessage(`{"async":true}`))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		TaskID string `json:"task_id"`
	}
	json.Unmarshal(raw, &reply)
	task := r.tasks.get(reply.TaskID)
	if task == nil || task.Tool != "restart" {
		t.Fatal("missing restart task")
	}
	select {
	case <-task.doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("restart did not finish")
	}
	data, _, _ := task.snapshot()
	if data.Error != "" || data.RestartResult == nil {
		t.Fatalf("restart failed: %+v", data)
	}
}

func TestExecPipesAndRedirects(t *testing.T) {
	cfg := mustCfg(t)
	r := NewRegistry(cfg, execshell.NewExecutor(cfg))
	file := filepath.Join(cfg.TomcatHome, "redirect.txt")
	command := `echo pipeline-ok | findstr pipeline > "` + file + `" 2>&1`
	if runtime.GOOS != "windows" {
		command = `printf pipeline-ok | cat > "` + file + `" 2>&1`
	}
	raw, err := r.execAction(context.Background(), mustMarshal(map[string]any{"cmd": command}))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ExitCode int `json:"exit_code"`
	}
	json.Unmarshal(raw, &result)
	if result.ExitCode != 0 {
		t.Fatal(string(raw))
	}
	b, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(b), "pipeline-ok") {
		t.Fatalf("redirect failed: %q %v", b, err)
	}
}
