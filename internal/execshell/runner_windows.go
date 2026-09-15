//go:build windows

package execshell

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// configureCmdLine makes the Windows runner hand the command string to
// cmd.exe verbatim instead of letting Go re-quote it.
//
// Background (2026-09-15 customer-box report, todo G2/G3): Go's os/exec
// builds the child command line by escaping every argument on its own
// (syscall.EscapeArg). cmd.exe does not understand backslash-escaped
// quotes, so `cmd /C <escaped>` shredded any quoting the caller wrote:
// "D:\Program Files\..." was split at the space and rejected as
// "path not allowed: D:\Program", tasklist /fi "..." lost its quotes,
// and the PowerShell -Command wrapper was echoed back instead of run.
//
// `cmd /d /s /c "<raw>"` is the documented way to pass an unmodified
// command string to cmd.exe: /s makes it strip exactly the outer pair of
// quotes and treat everything in between verbatim.
func configureCmdLine(cmd *exec.Cmd, name string, args []string) {
	if !isCmdExe(name) || len(args) < 2 || !strings.EqualFold(args[0], "/c") {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `/d /s /c "` + args[1] + `"`}
}

func isCmdExe(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return base == "cmd" || base == "cmd.exe"
}
