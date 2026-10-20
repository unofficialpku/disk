package vfs

import (
	"context"
	"errors"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/diagnostic"
)

// A directory listing already supplies revision, size and timestamps. Reuse
// that metadata until its TTL expires; a remote refresh never holds the node
// lock, so cached reads and open-handle attributes remain available.
func (n *Node) refreshMetadata(ctx context.Context) error {
	n.mu.Lock()
	if n.synthetic || n.dirty || n.unlinked || n.refs > 0 || time.Since(n.statAt) < n.f.opts.TTL {
		n.f.opts.Trace.Printf(ctx, "using local metadata (open-handles=%d pending-write=%t)", n.refs, n.dirty)
		n.mu.Unlock()
		return nil
	}
	if flight := n.statFlight; flight != nil {
		n.mu.Unlock()
		n.f.opts.Trace.Printf(ctx, "waiting for shared metadata refresh")
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	old := n.entry
	flight := &statFlight{done: make(chan struct{})}
	n.statFlight = flight
	n.mu.Unlock()
	ctx, finish := n.f.opts.Trace.Begin(ctx, "refresh metadata file=%q id=%q", old.Name, old.ID)
	ctx, cancel := context.WithTimeout(ctx, n.f.opts.IOTimeout)
	defer cancel()
	entry, err := n.f.backend.Stat(ctx, old)
	n.mu.Lock()
	namespaceChanged := false
	if n.entry == old && !n.dirty && !n.unlinked && n.cache == nil {
		if err == nil {
			n.entry, n.statAt = entry, time.Now()
			namespaceChanged = entry.ID != old.ID || entry.Name != old.Name
		} else if errors.Is(err, anyshare.ErrNotFound) {
			n.unlinked, namespaceChanged = true, true
		}
	} else {
		// A local write/rename or a newer listing superseded this response.
		n.f.opts.Trace.Printf(ctx, "metadata refresh superseded by local/newer file state")
		err = nil
	}
	flight.err = err
	n.statFlight = nil
	if namespaceChanged {
		n.f.invalidate()
	}
	close(flight.done)
	n.mu.Unlock()
	finish("%s", diagnostic.Status(err))
	return err
}
