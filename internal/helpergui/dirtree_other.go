//go:build !windows

package helpergui

import (
	"fmt"
	"os/exec"
)

// ListDrives is Windows-only; on other platforms the folder picker root
// starts at the user's home directory instead (dev convenience only —
// this product ships to Windows boxes).
func ListDrives() ([]string, error) {
	return nil, fmt.Errorf("drive enumeration is only supported on windows")
}

// runOpenBrowser uses xdg-open on non-Windows platforms (dev only).
func runOpenBrowser(url string) error {
	return exec.Command("xdg-open", url).Start()
}
