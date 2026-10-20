//go:build (linux && !android) || (darwin && !ios)

package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/vfs"
)

func Run(ctx context.Context, source *Source, opts Options, output io.Writer) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if output == nil {
		output = io.Discard
	}
	cache := localfile.ExpandPath(opts.Cache)
	readOnly, debug := opts.ReadOnly, opts.Debug
	cacheLimit := opts.CacheLimit
	ttl, timeout, refreshInterval := opts.TTL, opts.Timeout, opts.RefreshInterval
	point, err := filepath.Abs(localfile.ExpandPath(opts.Point))
	if err != nil {
		return err
	}
	info, err := os.Stat(point)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return syscall.ENOTDIR
	}
	contents, err := os.ReadDir(point)
	if err != nil {
		return err
	}
	if len(contents) != 0 {
		return errors.New("mountpoint must be an empty directory")
	}
	filesystem, err := vfs.New(source.Backend, vfs.Options{Root: source.Root, ReadOnly: readOnly, CacheDir: cache, CacheLimit: cacheLimit, Read: &opts.Read, TTL: ttl, IOTimeout: timeout, Trace: opts.Trace})
	if err != nil {
		return err
	}
	options := &fs.Options{MountOptions: fuse.MountOptions{Name: "pkudisk", FsName: "PKUDisk", Debug: debug, DisableXAttrs: true, MaxWrite: 1 << 20}, EntryTimeout: &ttl, AttrTimeout: &ttl, NegativeTimeout: new(time.Duration)}
	if readOnly {
		options.MountOptions.Options = []string{"ro"}
	}
	server, err := fs.Mount(point, filesystem.Root(), options)
	if err != nil {
		filesystem.Close()
		return fmt.Errorf("FUSE mount: %w", err)
	}
	fmt.Fprintf(output, "Mounted PKUDisk at %s; cache %s. Ctrl-C unmounts.\n", point, filesystem.CachePath())
	if opts.Ready != nil {
		opts.Ready()
	}
	maintenanceFailed, stopMaintenance := source.Maintain(ctx, refreshInterval, output)
	defer stopMaintenance()
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	for {
		select {
		case reply := <-opts.Unmount:
			if err := server.Unmount(); err != nil {
				reply <- err
				continue
			}
			<-done
			err := filesystem.Close()
			reply <- err
			return err
		case <-done:
			return filesystem.Close()
		case err := <-maintenanceFailed:
			if unmountErr := server.Unmount(); unmountErr != nil {
				return fmt.Errorf("%w; unmount failed: %w", err, unmountErr)
			}
			<-done
			return errors.Join(err, filesystem.Close())
		case <-ctx.Done():
			opts.Trace.Printf(ctx, "unmount requested; waiting for driver to release handles")
			if err := server.Unmount(); err != nil {
				return fmt.Errorf("unmount: %w", err)
			}
			<-done
			return filesystem.Close()
		}
	}
}
