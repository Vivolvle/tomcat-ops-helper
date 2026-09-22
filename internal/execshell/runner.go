package execshell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// OSRunner is the production Runner, backed by os/exec. It is stateless and
// deliberately permits concurrent commands: one slow process must not block
// every later exec call.
type OSRunner struct{ active sync.Map }

type RunningCommand struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Args    []string  `json:"args"`
	WorkDir string    `json:"work_dir"`
	Started time.Time `json:"started"`
}

func (r *OSRunner) Running() []RunningCommand {
	out := []RunningCommand{}
	r.active.Range(func(_, v any) bool { out = append(out, v.(RunningCommand)); return true })
	return out
}

func (e *Executor) Running() []RunningCommand {
	if r, ok := e.runner.(interface{ Running() []RunningCommand }); ok {
		return r.Running()
	}
	return []RunningCommand{}
}

// NewOSRunner constructs a fresh OSRunner.
func NewOSRunner() *OSRunner { return &OSRunner{} }

// Run implements Runner. It captures stdout and stderr separately, applies
// context for timeout, and packs the exit code into Result.
func (r *OSRunner) Run(ctx context.Context, name string, args []string, workDir string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCmdLine(cmd, name, args)
	configureCancellation(cmd)
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	r.active.Store(cmd.Process.Pid, RunningCommand{cmd.Process.Pid, name, append([]string(nil), args...), workDir, time.Now()})
	defer r.active.Delete(cmd.Process.Pid)
	err := cmd.Wait()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("exec: %w", err)
	}
	res.ExitCode = 0
	return res, nil
}
