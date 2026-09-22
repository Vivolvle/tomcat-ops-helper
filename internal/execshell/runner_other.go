//go:build !windows

package execshell

import "os/exec"

// configureCmdLine is a no-op outside Windows; Go's own argument
// escaping is correct for sh -c.
func configureCmdLine(cmd *exec.Cmd, name string, args []string) {}

// CommandContext already kills the direct process on non-Windows platforms.
// WaitDelay prevents inherited pipe handles from holding Wait indefinitely.
func configureCancellation(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
