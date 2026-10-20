//go:build darwin && !ios

package mount

import (
	"errors"
	"os"
)

const PlatformHelp = "macOS requires macFUSE (https://macfuse.github.io/) with its kernel extension\nenabled in System Settings. Use an existing empty directory. Ctrl-C unmounts."
const PlatformExample = "  mkdir -p ~/PKUDisk\n  pkudisk mount ~/PKUDisk\n  pkudisk mount ~/PKUDisk --account work --read-only\n  pkudisk mount ~/PKUDisk --root '/My library/Reports'"
const PlatformPoint = "~/PKUDisk"

func CheckSupport() error {
	for _, utility := range []string{
		"/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse",
		"/Library/Filesystems/osxfuse.fs/Contents/Resources/mount_osxfuse",
	} {
		if info, err := os.Stat(utility); err == nil && !info.IsDir() {
			return nil
		}
	}
	return errors.New("mount requires macFUSE with its kernel extension enabled; install it from https://macfuse.github.io/; account, ls, upload, download and tui work without it")
}
