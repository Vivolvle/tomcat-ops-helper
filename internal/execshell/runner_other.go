//go:build !windows

package execshell

import "os/exec"

// configureCmdLine is a no-op outside Windows; Go's own argument
// escaping is correct for sh -c.
func configureCmdLine(cmd *exec.Cmd, name string, args []string) {}
