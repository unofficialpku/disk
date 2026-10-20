package vfs

import (
	"context"
	"io"
	"os"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/diagnostic"
)

func (r *readFlight) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(data) > len(r.data)-r.written {
		return 0, io.ErrShortWrite
	}
	copy(r.data[r.written:], data)
	r.written += len(data)
	close(r.changed)
	r.changed = make(chan struct{})
	return len(data), nil
}

func (r *readFlight) read(ctx context.Context, trace *diagnostic.Logger, data []byte, off int64) (int, error) {
	start := off - r.off
	end := start + int64(len(data))
	for {
		r.mu.Lock()
		if r.finished && r.err != nil {
			err := r.err
			r.mu.Unlock()
			return 0, err
		}
		// Reads at the end of the range wait for final byte-count/EOF validation.
		// Earlier demand can use its immutable prefix while the suffix fills.
		if int64(r.written) >= end && (end < r.length || r.finished) {
			n := copy(data, r.data[start:end])
			filling := !r.finished
			r.mu.Unlock()
			if filling {
				trace.Printf(ctx, "requested bytes ready: returned=%s; shared range continues filling", trace.Size(int64(n)))
			}
			return n, nil
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (f *FileSystem) fetchRange(entry anyshare.Entry, flight *readFlight) {
	defer f.readWG.Done()
	// A caller cancelling its read must not interrupt another reader's shared
	// transfer. The bounded fetch ends on its own deadline or mount shutdown.
	defer flight.cancel()
	defer flight.stop()
	ctx, finish := f.opts.Trace.Begin(flight.ctx, "fetch file=%q id=%q revision=%q offset=%d length=%s blocks=%d", entry.Name, entry.ID, entry.Rev, flight.off, f.opts.Trace.Size(flight.length), len(flight.keys))
	f.opts.Trace.Printf(ctx, "cache miss; waiting for read slot (active=%d limit=%d)", len(f.readSlots), cap(f.readSlots))
	err := f.acquireRead(ctx)
	admitted := err == nil
	if admitted {
		defer func() { <-f.readSlots }()
		f.opts.Trace.Printf(ctx, "read slot acquired; streaming remote bytes %d-%d", flight.off, flight.off+flight.length-1)
		flight.mu.Lock()
		flight.data = make([]byte, flight.length)
		flight.mu.Unlock()
		err = f.backend.ReadRange(ctx, entry, flight.off, flight.length, flight)
	}
	flight.mu.Lock()
	n := flight.written
	flight.mu.Unlock()
	if err == nil && int64(n) != flight.length {
		err = io.ErrUnexpectedEOF
	}
	f.opts.Trace.Printf(ctx, "remote range finished: received=%s; %s; waiting for cache lock", f.opts.Trace.Size(int64(n)), diagnostic.Status(err))
	f.budgetMu.Lock()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && !f.closed && f.cacheRoomLocked(int64(n)) {
		file, createErr := os.CreateTemp(f.cacheDir, "read-*.data")
		if createErr == nil {
			f.opts.Trace.Printf(ctx, "retaining range in disk cache: size=%s", f.opts.Trace.Size(int64(n)))
			_, writeErr := file.Write(flight.data)
			closeErr := file.Close()
			if writeErr == nil && closeErr == nil {
				f.cacheBytes += int64(n)
				f.readBytes += int64(n)
				element := f.readLRU.PushBack(cachedFile{key: flight.keys[0], path: file.Name(), size: int64(n), keys: flight.keys})
				for _, key := range flight.keys {
					f.readFiles[key] = element
				}
				f.opts.Trace.Printf(ctx, "cache range retained: used=%s clean=%s", f.opts.Trace.Size(f.cacheBytes), f.opts.Trace.Size(f.readBytes))
			} else {
				f.opts.Trace.Printf(ctx, "disk retention failed; fetched range remains available to its readers")
				os.Remove(file.Name())
			}
		} else {
			f.opts.Trace.Printf(ctx, "disk cache file creation failed: %s; returning fetched bytes without retention", diagnostic.Status(createErr))
		}
	} else if err == nil {
		f.opts.Trace.Printf(ctx, "range not retained: shared cache could not free required space or mount is closing")
	}
	for _, key := range flight.keys {
		if f.readFlights[key] == flight {
			delete(f.readFlights, key)
		}
	}
	flight.mu.Lock()
	flight.err, flight.finished = err, true
	close(flight.done)
	close(flight.changed)
	flight.mu.Unlock()
	f.budgetMu.Unlock()
	finish("%s", diagnostic.Status(err))
}
