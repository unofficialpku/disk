package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/vfs"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const PlatformHelp = "Windows requires WinFsp (https://winfsp.dev/rel/). Choose an unused drive\nletter such as P:, or an empty local directory. Ctrl-C unmounts."
const PlatformExample = "  pkudisk mount P:\n  pkudisk mount P: --account work --read-only\n  pkudisk mount P: --root '/My library/Reports'"
const PlatformPoint = "P:"

func CheckSupport() error {
	dllName := "winfsp-x64.dll"
	if runtime.GOARCH == "arm64" {
		dllName = "winfsp-a64.dll"
	}
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.QUERY_VALUE|view)
		if err != nil {
			continue
		}
		dir, _, err := key.GetStringValue("InstallDir")
		key.Close()
		if err != nil {
			continue
		}
		dll, err := windows.LoadDLL(filepath.Join(dir, "bin", dllName))
		if err == nil {
			dll.Release()
			return nil
		}
	}
	return errors.New("mount requires WinFsp for this architecture; install it from https://winfsp.dev/rel/, then run mount again")
}

func windowsMountpoint(value string) (string, error) {
	if len(value) == 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) {
		value = strings.ToUpper(value)
		drives, err := windows.GetLogicalDrives()
		if err != nil {
			return "", err
		}
		if drives&(1<<uint(value[0]-'A')) != 0 {
			return "", fmt.Errorf("drive %s is already in use; choose an unused drive letter", value)
		}
		return value, nil
	}
	point, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(point)
	if errors.Is(err, os.ErrNotExist) {
		return point, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("mountpoint must be an unused drive letter or an empty directory")
	}
	entries, err := os.ReadDir(point)
	if err != nil {
		return "", err
	}
	if len(entries) > 0 {
		return "", errors.New("mountpoint directory must be empty")
	}
	return point, nil
}

func Run(ctx context.Context, source *Source, opts Options, output io.Writer) (result error) {
	if err := opts.Validate(); err != nil {
		return err
	}
	if output == nil {
		output = io.Discard
	}
	point, err := windowsMountpoint(localfile.ExpandPath(opts.Point))
	if err != nil {
		return err
	}
	core, err := vfs.New(source.Backend, vfs.Options{Root: source.Root, ReadOnly: opts.ReadOnly, CacheDir: localfile.ExpandPath(opts.Cache), CacheLimit: opts.CacheLimit, Read: &opts.Read, TTL: opts.TTL, IOTimeout: opts.Timeout, Trace: opts.Trace})
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, core.Close()) }()
	// Keep the operation context alive until the driver finishes releasing its
	// handles, so Ctrl-C can unmount without cancelling an in-flight flush.
	operationCtx, stopOperations := context.WithCancel(context.Background())
	defer stopOperations()
	adapter := vfs.NewWinFsp(operationCtx, core)
	host := fuse.NewFileSystemHost(adapter)
	host.SetCapOpenTrunc(true)
	host.SetCapReaddirPlus(true)
	host.SetUseIno(true)
	arguments := append(vfs.WinFspMountOptions(), "-o", fmt.Sprintf("fsname=PKUDisk,volname=PKUDisk,attr_timeout=%g,entry_timeout=%g", opts.TTL.Seconds(), opts.TTL.Seconds()))
	if opts.ReadOnly {
		arguments = append(arguments, "-o", "ro")
	}
	if opts.Debug {
		arguments = append(arguments, "-d")
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("WinFsp mount failed: %v", recovered)
			}
		}()
		if !host.Mount(point, arguments) {
			done <- errors.New("WinFsp mount failed; check the driver and choose an unused drive letter or empty directory")
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-adapter.Ready:
	}
	fmt.Fprintf(output, "Mounted PKUDisk at %s; cache %s. Ctrl-C unmounts.\n", point, core.CachePath())
	if opts.Ready != nil {
		opts.Ready()
	}
	maintenanceFailed, stopMaintenance := source.Maintain(ctx, opts.RefreshInterval, output)
	defer stopMaintenance()
	for {
		select {
		case reply := <-opts.Unmount:
			if !host.Unmount() {
				reply <- errors.New("WinFsp unmount failed; close files and try again")
				continue
			}
			result = <-done
			reply <- result
			return result
		case err := <-done:
			return err
		case result = <-maintenanceFailed:
		case <-ctx.Done():
		}
		opts.Trace.Printf(ctx, "unmount requested; waiting for WinFsp to release handles")
		if !host.Unmount() {
			return errors.Join(result, errors.New("WinFsp unmount failed"))
		}
		return errors.Join(result, <-done)
	}
}
