// Package diagnostic records mount decisions and request lifetimes without
// logging request bodies, credentials or signed object URLs.
package diagnostic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
)

type Logger struct {
	log      *log.Logger
	next     atomic.Uint64
	rawBytes atomic.Bool
}

type operationKey struct{}

func New(out io.Writer) *Logger {
	return &Logger{log: log.New(out, "pkudisk: ", log.LstdFlags|log.Lmicroseconds)}
}

// A nil logger is disabled. Keep the original context and avoid allocating a
// timer, span or closure for ordinary mounts.
func (l *Logger) Printf(ctx context.Context, format string, args ...any) {
	if l == nil {
		return
	}
	id, _ := ctx.Value(operationKey{}).(uint64)
	l.log.Printf("[#%d] %s", id, fmt.Sprintf(format, args...))
}

func noop(string, ...any) {}

func (l *Logger) Begin(ctx context.Context, format string, args ...any) (context.Context, func(string, ...any)) {
	if l == nil {
		return ctx, noop
	}
	parent, _ := ctx.Value(operationKey{}).(uint64)
	id := l.next.Add(1)
	ctx = context.WithValue(ctx, operationKey{}, id)
	description := fmt.Sprintf(format, args...)
	started := time.Now()
	l.Printf(ctx, "START %s (parent=#%d)", description, parent)
	return ctx, func(format string, args ...any) {
		l.Printf(ctx, "END %s: %s; elapsed=%s", description, fmt.Sprintf(format, args...), time.Since(started).Round(time.Microsecond))
	}
}

// SetBytes is configured before the mount starts issuing requests.
func (l *Logger) SetBytes(enabled bool) {
	if l != nil {
		l.rawBytes.Store(enabled)
	}
}

// Size delays formatting until an enabled diagnostic logger consumes it.
func (l *Logger) Size(n int64) fmt.Stringer {
	return sizeValue{n: n, raw: l != nil && l.rawBytes.Load()}
}

type sizeValue struct {
	n   int64
	raw bool
}

func (s sizeValue) String() string {
	if s.n < 0 {
		return "unknown"
	}
	if s.raw {
		return fmt.Sprintf("%d bytes", s.n)
	}
	return humanize.IBytes(uint64(s.n))
}

type Errno syscall.Errno

func (e Errno) String() string {
	if e == 0 {
		return "success"
	}
	return syscall.Errno(e).Error()
}

// Errors from a transport or response parser may embed a URL or body. Report
// known error classes, never arbitrary error strings.
func Status(err error) string {
	if err == nil {
		return "success"
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF, io.EOF} {
		if errors.Is(err, known) {
			return "failed: " + known.Error()
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return "failed: " + errno.Error()
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "failed: network timeout"
	}
	return fmt.Sprintf("failed (%T)", err)
}
