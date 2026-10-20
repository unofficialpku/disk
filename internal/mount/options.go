// Package mount owns account-backed filesystem mounting and its shared policy.
// Callers supply already connected sessions or a headless account connector.
package mount

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"github.com/unofficialpku/disk/internal/localfile"
	"github.com/unofficialpku/disk/internal/vfs"
)

var ErrUnsupported = errors.New("filesystem mounting is unavailable on this platform")

type Options struct {
	Ready                         func()
	Unmount                       <-chan chan error
	Point, Root, Cache            string
	ReadOnly, Debug, Verbose      bool
	CacheLimit, PartSize          int64
	TTL, Timeout, RefreshInterval time.Duration
	Read                          vfs.ReadOptions
	Trace                         *diagnostic.Logger
}

func DefaultCache() string {
	dir, err := localfile.CacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "pkudisk")
}

func DefaultOptions() Options {
	return Options{Cache: DefaultCache(), CacheLimit: 10 << 30, PartSize: 16 << 20,
		Read: vfs.DefaultReadOptions(), TTL: 2 * time.Second, Timeout: 10 * time.Minute,
		RefreshInterval: time.Hour}
}

func (o Options) Validate() error {
	if o.CacheLimit < 0 || o.PartSize < 5<<20 || o.TTL < 0 || o.RefreshInterval < 0 {
		return errors.New("cache limits and intervals must be nonnegative; --part-size must be at least 5242880 bytes")
	}
	if o.Timeout <= 0 {
		return errors.New("I/O timeout must be positive")
	}
	return o.Read.Validate()
}

// Input is the string/seconds representation accepted by an interactive surface.
// Pointer intervals and string sizes distinguish an explicit zero from defaults.
type Input struct {
	Point, Root, Cache                                    string
	ReadOnly, Debug, Verbose                              bool
	CacheLimit, PartSize, WholeFile, BlockSize, ReadAhead string
	Concurrency, TimeoutSeconds                           int
	TTLSeconds, RefreshSeconds                            *int
}

func ParseOptions(input Input) (Options, error) {
	opts := DefaultOptions()
	opts.Point, opts.Root = input.Point, input.Root
	opts.ReadOnly, opts.Debug, opts.Verbose = input.ReadOnly, input.Debug, input.Verbose
	if input.Cache != "" {
		opts.Cache = input.Cache
	}
	for _, field := range []struct {
		text   string
		target *int64
	}{
		{input.CacheLimit, &opts.CacheLimit}, {input.PartSize, &opts.PartSize},
		{input.WholeFile, &opts.Read.WholeFile}, {input.BlockSize, &opts.Read.BlockSize}, {input.ReadAhead, &opts.Read.ReadAhead},
	} {
		if field.text != "" {
			if err := (ByteSize{Value: field.target}).Set(field.text); err != nil {
				return opts, err
			}
		}
	}
	if input.Concurrency != 0 {
		opts.Read.Concurrency = input.Concurrency
	}
	if input.TimeoutSeconds != 0 {
		if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 86400 {
			return opts, errors.New("invalid I/O timeout")
		}
		opts.Timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}
	for _, field := range []struct {
		seconds *int
		target  *time.Duration
		name    string
	}{
		{input.TTLSeconds, &opts.TTL, "metadata TTL"}, {input.RefreshSeconds, &opts.RefreshInterval, "refresh interval"},
	} {
		if field.seconds != nil {
			if *field.seconds < 0 || *field.seconds > 86400 {
				return opts, fmt.Errorf("invalid %s", field.name)
			}
			*field.target = time.Duration(*field.seconds) * time.Second
		}
	}
	return opts, opts.Validate()
}

// ByteSize implements pflag.Value with the same syntax for every mount size.
type ByteSize struct{ Value *int64 }

func (b ByteSize) String() string { return humanize.IBytes(uint64(*b.Value)) }
func (b ByteSize) Type() string   { return "size" }
func (b ByteSize) Set(text string) error {
	if size, err := strconv.ParseInt(text, 10, 64); err == nil && size >= 0 {
		*b.Value = size
		return nil
	}
	size, err := humanize.ParseBytes(text)
	if err != nil || size > math.MaxInt64 {
		return fmt.Errorf("invalid size %q; use bytes or units such as 4MiB and 10GiB", text)
	}
	*b.Value = int64(size)
	return nil
}
