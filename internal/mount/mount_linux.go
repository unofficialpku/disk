//go:build linux && !android

package mount

import (
	"errors"
	"os"
	"os/exec"
)

const PlatformHelp = "Linux requires /dev/fuse and fusermount3/fusermount (install your distribution's\nfuse3 package). Use an existing empty directory. Ctrl-C unmounts."
const PlatformExample = "  mkdir -p ~/PKUDisk\n  pkudisk mount ~/PKUDisk\n  pkudisk mount ~/PKUDisk --account work --read-only\n  pkudisk mount ~/PKUDisk --root '/My library/Reports'"
const PlatformPoint = "~/PKUDisk"

func CheckSupport() error {
	file, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return errors.New("mount requires access to /dev/fuse; enable FUSE and install your distribution's fuse3 package")
	}
	file.Close()
	if _, err := exec.LookPath("fusermount3"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("fusermount"); err == nil {
		return nil
	}
	return errors.New("mount requires fusermount3 or fusermount; install your distribution's fuse3 package")
}
