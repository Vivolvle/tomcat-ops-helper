package actions

// Regression test for the 2026-10-03 problem.md report (liang/claude,
// yc-domains deploy of the mbi jar): procrun's slow shutdown leaves the
// service in STOP_PENDING for a minute after the JVM exits, and sc start
// during that window returns 1056. start_service must wait for STOPPED
// and retry instead of failing the deploy.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tomcat-ops-helper/internal/execshell"
)

// TestStartService_1056DuringStopPendingWaitsAndRetries: 1056 + state
// STOP_PENDING → poll to STOPPED → one retry sc start → RUNNING = success.
func TestStartService_1056DuringStopPendingWaitsAndRetries(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	runner := &scriptRunner{rules: []scriptRule{
		// First sc start hits 1056 (service mid-stop); the retry after
		// waiting for STOPPED succeeds.
		{match: `sc start "Tomcat10ESB"`, stderr: "SC StartService FAILED 1056: An instance of the service is already running.", failFirst: 1},
		// The 1056 re-check sees STOP_PENDING; the wait-for-STOPPED poll
		// sees STOPPED; the wait-for-RUNNING poll sees RUNNING.
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 3  STOP_PENDING\n",
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 1  STOPPED\n",
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 4  RUNNING\n",
		}},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.startService(context.Background(), mustMarshal(map[string]any{}))
	if err != nil {
		t.Fatalf("1056 during STOP_PENDING must be retried to success, got %v", err)
	}
	var got StartServiceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Running || got.AlreadyRunning {
		t.Fatalf("expected Running=true AlreadyRunning=false (it was stopping, not running), got %+v", got)
	}
	if !strings.Contains(got.Note, "STOPPED") {
		t.Fatalf("note should explain the slow-stop retry: %q", got.Note)
	}
	// Exactly two sc start attempts: the 1056 victim plus the retry.
	starts := 0
	for _, c := range runner.calls {
		if strings.Contains(c, `sc start "Tomcat10ESB"`) {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("expected exactly 2 sc start calls, got %d: %v", starts, runner.calls)
	}
}

// TestStartService_1056NeverReachesStoppedFails: when the service never
// leaves STOP_PENDING within the budget the error must say so instead of
// looping forever or misreporting.
func TestStartService_1056NeverReachesStoppedFails(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc start "Tomcat10ESB"`, stderr: "SC StartService FAILED 1056: An instance of the service is already running.", failFirst: 1000},
		// Stuck in STOP_PENDING forever (poll clamps to the last stdout).
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 3  STOP_PENDING\n",
		}},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	_, err := reg.startService(context.Background(), mustMarshal(map[string]any{"timeout": 2}))
	if err == nil {
		t.Fatal("stuck STOP_PENDING must fail, not succeed")
	}
	if !strings.Contains(err.Error(), "STOPPED") {
		t.Fatalf("error should mention never reaching STOPPED: %v", err)
	}
}
