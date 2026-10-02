//go:build windows

package helpergui

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW    = kernel32.NewProc("GetDriveTypeW")
	procShellExecuteW    = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
)

// ListDrives enumerates fixed and removable volumes as `C:\`-shaped
// paths for the folder picker root. CD-ROM drives are skipped (never a
// config target).
func ListDrives() ([]string, error) {
	rc, _, err := procGetLogicalDrives.Call()
	if rc == 0 {
		return nil, fmt.Errorf("GetLogicalDrives: %v", err)
	}
	mask := uint32(rc)
	var drives []string
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := byte('A' + i)
		root := string([]byte{letter, ':', '\\'})
		rootPtr, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		// DRIVE_CDROM = 5
		if t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(rootPtr))); t == 5 {
			continue
		}
		drives = append(drives, root)
	}
	return drives, nil
}

// runOpenBrowser opens url via ShellExecuteW (more robust than
// `cmd /C start` for URLs with query strings).
func runOpenBrowser(url string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	target, _ := syscall.UTF16PtrFromString(url)
	rc, _, err := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, syscall.SW_SHOWNORMAL)
	if rc <= 32 {
		return fmt.Errorf("ShellExecuteW(%q): %v", url, err)
	}
	return nil
}
