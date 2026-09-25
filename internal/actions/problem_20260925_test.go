package actions

// Regression tests for the second 2026-09-25 problem.md round (19:03/19:31
// reports from claude-shunyi-yc-esb): exec timeout cap, JVM-wait fallback
// to sc query, 1056 idempotent start, JVM-probe false positives, and the
// async deploy task model.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tomcat-ops-helper/internal/execshell"
)

// TestStopService_ProbeErrorFallsBackToScQuery covers the WIN-JRD8SLQTK01
// report: JVM enumeration unavailable (sick WMI) must not fail stop_service
// nor wedge the wait — sc query STOPPED confirms the exit instead.
func TestStopService_ProbeErrorFallsBackToScQuery(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc stop "Tomcat10ESB"`},
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 1  STOPPED\n",
		}},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{javaErr: errors.New("wmic: broken")})

	raw, err := reg.stopService(context.Background(), mustMarshal(map[string]any{}))
	if err != nil {
		t.Fatalf("stop_service must not fail when only the probe is broken: %v", err)
	}
	var got StopServiceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Exited {
		t.Fatalf("sc query STOPPED should confirm exit, got %+v", got)
	}
}

// TestWaitCatalina_GoneWithProbeErrorScFallback: state=gone + broken
// enumeration + service STOPPED returns success with an explanatory note
// instead of hanging until the timeout.
func TestWaitCatalina_GoneWithProbeErrorScFallback(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 1  STOPPED\n",
		}},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{javaErr: errors.New("powershell: WMI sick")})

	raw, err := reg.waitCatalina(context.Background(), mustMarshal(map[string]any{"state": "gone", "timeout": 5}))
	if err != nil {
		t.Fatalf("waitCatalina: %v", err)
	}
	var got WaitCatalinaData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Exited || got.TimedOut {
		t.Fatalf("expected Exited=true via sc fallback, got %+v", got)
	}
	if !strings.Contains(got.Note, "sc query") {
		t.Fatalf("note should mention the sc query fallback: %q", got.Note)
	}
}

// TestStartService_1056CountsAsRunning covers the deploy-race report: sc
// start returning 1056 ("An instance of the service is already running")
// must re-check the state and report success when RUNNING, not fail.
func TestStartService_1056CountsAsRunning(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc start "Tomcat10ESB"`, stderr: "SC StartService FAILED 1056: An instance of the service is already running.", failFirst: 1000},
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 4  RUNNING\n",
		}},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.startService(context.Background(), mustMarshal(map[string]any{}))
	if err != nil {
		t.Fatalf("1056 with state RUNNING must be success, got %v", err)
	}
	var got StartServiceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Running || !got.AlreadyRunning {
		t.Fatalf("expected Running=true AlreadyRunning=true, got %+v", got)
	}
}

// TestLooksLikeJVM_ExcludesMonitorAndCmdlineNoise pins the false-positive
// fixes: the procrun monitor (…w.exe) always runs and must not look like
// the JVM, and non-Java processes whose command line merely mentions
// catalina (the operator's own log-reading commands) must not either.
func TestLooksLikeJVM_ExcludesMonitorAndCmdlineNoise(t *testing.T) {
	cases := []struct {
		name string
		p    JavaProcess
		want bool
	}{
		{"java exe", JavaProcess{Name: "java.exe", Cmdline: "java -Dcatalina.home=D:\\tomcat"}, true},
		{"prunsrv service binary", JavaProcess{Name: "tomcat10-esb.exe", Cmdline: "D:\\bs-esb\\bin\\tomcat10-esb.exe //RS//tomcat10-esb"}, true},
		{"prunsrv monitor gui", JavaProcess{Name: "tomcat10-esbw.exe", Cmdline: "D:\\bs-esb\\bin\\tomcat10-esbw.exe //MR//tomcat10-esb"}, false},
		{"operator reading catalina log", JavaProcess{Name: "powershell.exe", Cmdline: "powershell Get-Content D:\\tomcat\\logs\\catalina.2026-09-25.log -Tail 50"}, false},
		{"editor with log open", JavaProcess{Name: "notepad.exe", Cmdline: "notepad.exe D:\\tomcat\\logs\\catalina.out"}, false},
		{"nameless cmdline fallback", JavaProcess{Cmdline: "java -Dcatalina.base=D:\\tomcat"}, true},
		{"boring process", JavaProcess{Name: "explorer.exe", Cmdline: "C:\\Windows\\explorer.exe"}, false},
	}
	for _, tc := range cases {
		if got := looksLikeJVM(tc.p); got != tc.want {
			t.Errorf("%s: looksLikeJVM=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParseTasklistCSV covers the WMI-free last-resort enumerator.
func TestParseTasklistCSV(t *testing.T) {
	out := "\"tomcat10-esb.exe\",\"4242\",\"Console\",\"1\",\"51,240 K\"\n" +
		"\"java.exe\",\"1111\",\"Console\",\"1\",\"2,048 K\"\n" +
		"\"explorer.exe\",\"2222\",\"Console\",\"1\",\"80,096 K\"\n"
	procs := filterInterestingProcesses(parseTasklistCSV(out))
	if len(procs) != 2 {
		t.Fatalf("expected tomcat10-esb + java kept, got %+v", procs)
	}
	for _, p := range procs {
		if p.PID == 2222 {
			t.Fatalf("explorer must not survive the filter: %+v", procs)
		}
	}
}

// TestDeploy_AsyncReturnsTaskHandleThenCompletes covers the async task
// model: async:true returns a task_id immediately; the cycle finishes in
// the background; task_status reports done with all three step results;
// the no-task_id listing finds it; an unknown id errors.
func TestDeploy_AsyncReturnsTaskHandleThenCompletes(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	seedDeployJars(t, cfg)

	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc stop "Tomcat10ESB"`},
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 4  RUNNING\n",
		}},
		{match: `sc start "Tomcat10ESB"`},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.deploy(context.Background(), mustMarshal(map[string]any{
		"src":           filepath.Join(cfg.TomcatHome, "uploads", "app.jar"),
		"dest":          filepath.Join(cfg.TomcatHome, "webapps", "app.jar"),
		"async":         true,
		"stop_timeout":  5,
		"start_timeout": 5,
	}))
	if err != nil {
		t.Fatalf("deploy async: %v", err)
	}
	var handle struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &handle); err != nil {
		t.Fatalf("unmarshal handle: %v", err)
	}
	if handle.TaskID == "" || handle.Status != "running" {
		t.Fatalf("expected running task handle, got %+v", handle)
	}

	// Poll task_status until done (bounded — a hang here fails the test).
	deadline := time.Now().Add(10 * time.Second)
	var status TaskStatusData
	for {
		sraw, err := reg.taskStatus(context.Background(), mustMarshal(map[string]any{"task_id": handle.TaskID}))
		if err != nil {
			t.Fatalf("taskStatus: %v", err)
		}
		if err := json.Unmarshal(sraw, &status); err != nil {
			t.Fatalf("unmarshal status: %v", err)
		}
		if status.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not finish in 10s: %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status.Deploy == nil || status.Deploy.StopResult == nil ||
		status.Deploy.ReplaceResult == nil || status.Deploy.StartResult == nil {
		t.Fatalf("expected all three step results, got %+v", status.Deploy)
	}
	if status.Deploy.Error != "" || status.Deploy.FinalState != "RUNNING" {
		t.Fatalf("expected clean deploy, got error=%q final=%q", status.Deploy.Error, status.Deploy.FinalState)
	}
	if status.Deploy.TaskID != handle.TaskID {
		t.Fatalf("deploy task_id mismatch: %q vs %q", status.Deploy.TaskID, handle.TaskID)
	}

	// Listing without task_id must include the task.
	lraw, err := reg.taskStatus(context.Background(), mustMarshal(map[string]any{}))
	if err != nil {
		t.Fatalf("taskStatus list: %v", err)
	}
	var list struct {
		Tasks []TaskSummaryData `json:"tasks"`
	}
	if err := json.Unmarshal(lraw, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	found := false
	for _, s := range list.Tasks {
		if s.TaskID == handle.TaskID {
			found = s.Done
		}
	}
	if !found {
		t.Fatalf("task %s not in listing: %+v", handle.TaskID, list.Tasks)
	}

	// Unknown id → clear error.
	if _, err := reg.taskStatus(context.Background(), mustMarshal(map[string]any{"task_id": "nope"})); err == nil {
		t.Fatal("unknown task_id should error")
	}
}

// TestDeploy_SyncStillReturnsTaskID ensures the synchronous contract kept
// working through the task wrapper: same fields as before, plus task_id.
func TestDeploy_SyncStillReturnsTaskID(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	seedDeployJars(t, cfg)
	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc stop "Tomcat10ESB"`},
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 4  RUNNING\n",
		}},
		{match: `sc start "Tomcat10ESB"`},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.deploy(context.Background(), mustMarshal(map[string]any{
		"src":  filepath.Join(cfg.TomcatHome, "uploads", "app.jar"),
		"dest": filepath.Join(cfg.TomcatHome, "webapps", "app.jar"),
	}))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var got DeployData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TaskID == "" {
		t.Fatal("sync deploy must also carry task_id")
	}
	if got.StopResult == nil || got.ReplaceResult == nil || got.StartResult == nil {
		t.Fatalf("sync deploy must return all step results, got %+v", got)
	}
}

// TestDeploy_SyncPartialOnStopFailureKeepsTaskEntry: a failed synchronous
// deploy is still listed in task_status so a caller that only saw a
// transport timeout can find out how far it got.
func TestDeploy_SyncPartialOnStopFailureKeepsTaskEntry(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc stop "Tomcat10ESB"`, failFirst: 1000, stderr: "stop refused"},
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 4  RUNNING\n",
		}},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.deploy(context.Background(), mustMarshal(map[string]any{
		"src": "a.jar", "dest": "b.jar",
	}))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var got DeployData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Step != "stop_service" || got.Error == "" {
		t.Fatalf("expected stop failure recorded, got %+v", got)
	}
	lraw, err := reg.taskStatus(context.Background(), mustMarshal(map[string]any{"task_id": got.TaskID}))
	if err != nil {
		t.Fatalf("taskStatus for %s: %v", got.TaskID, err)
	}
	var st TaskStatusData
	if err := json.Unmarshal(lraw, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !st.Done || st.Deploy == nil || st.Deploy.Step != "stop_service" {
		t.Fatalf("expected done task with stop_service step, got %+v", st)
	}
}
