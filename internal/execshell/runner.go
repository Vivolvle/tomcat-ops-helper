package execshell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

// OSRunner is the production Runner, backed by os/exec.
type OSRunner struct {
	mu sync.Mutex
}

// NewOSRunner constructs a fresh OSRunner.
func NewOSRunner() *OSRunner { return &OSRunner{} }

// Run implements Runner. It captures stdout and stderr separately, applies
// context for timeout, and packs the exit code into Result.
func (r *OSRunner) Run(ctx context.Context, name string, args []string, workDir string) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := exec.CommandContext(ctx, name, args...)
	configureCmdLine(cmd, name, args)
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
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
