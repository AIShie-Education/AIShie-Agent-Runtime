package registry

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Defaults of the Watcher's timing.
const (
	// DefaultPoll is how often the registry's revision is read, should a
	// notification be lost.
	DefaultPoll = 30 * time.Second
	// DefaultRetry and DefaultRetryMax pace listening again after the
	// listener's connection fails.
	DefaultRetry    = time.Second
	DefaultRetryMax = 30 * time.Second
	// DefaultTimeout bounds each read of the revision and each rebuild.
	DefaultTimeout = 10 * time.Second
)

// Watcher puts the registry's changes in force: it rebuilds the runtime's
// configuration at each notification of a listener (pgstore's LISTEN
// aishie_registry, on a connection of its own, connected again whenever it
// fails), and whenever a poll of the registry's revision finds it moved on
// from the one the configuration in force was built from, should a
// notification be lost.
type Watcher struct {
	// Rev reads the registry's revision.
	Rev func(ctx context.Context) (int64, error)
	// Listen listens until ctx is done or it fails, calling ready once it
	// listens and changed at each notification:
	// pgstore.Store.ListenRegistry. Nil, the watcher only polls.
	Listen func(ctx context.Context, ready, changed func()) error
	// Rebuild builds the configuration from the registry, puts it in
	// force, and returns the revision it was built from.
	Rebuild func(ctx context.Context) (int64, error)
	// Poll is how often the revision is read; DefaultPoll when zero.
	Poll time.Duration
	// Retry is the first wait before listening again after a failure,
	// doubling to RetryMax; DefaultRetry and DefaultRetryMax when zero.
	Retry, RetryMax time.Duration
	// Timeout bounds each read of the revision and each rebuild, so that
	// a database that does not answer (a lock held, a connection lost
	// without a word) holds up neither the poll nor the notifications
	// after it; DefaultTimeout when zero.
	Timeout time.Duration
	// Log is where failures are logged; nowhere when nil.
	Log *slog.Logger
}

// Run watches until ctx is done, from rev, the revision the configuration
// in force was built from, and returns once its listener has stopped too.
func (w *Watcher) Run(ctx context.Context, rev int64) {
	log := w.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	kick := make(chan struct{}, 1)
	poke := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	var wg sync.WaitGroup
	if w.Listen != nil {
		wg.Go(func() { w.listen(ctx, log, poke) })
	}
	defer wg.Wait()
	poll := time.NewTicker(or(w.Poll, DefaultPoll))
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
			rev = w.rebuild(ctx, log, rev, "a notification")
		case <-poll.C:
			now, err := w.rev(ctx)
			switch {
			case err != nil:
				if ctx.Err() == nil {
					log.Warn("the registry's revision could not be read; it is read again at the next poll", "err", err)
				}
			case now != rev:
				rev = w.rebuild(ctx, log, rev, "the poll")
			}
		}
	}
}

// rev reads the registry's revision, within Timeout.
func (w *Watcher) rev(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, or(w.Timeout, DefaultTimeout))
	defer cancel()
	return w.Rev(ctx)
}

// rebuild rebuilds, within Timeout, and returns the revision now in force:
// last, if the registry could not be read, so that the next poll tries
// again.
func (w *Watcher) rebuild(ctx context.Context, log *slog.Logger, last int64, why string) int64 {
	rctx, cancel := context.WithTimeout(ctx, or(w.Timeout, DefaultTimeout))
	defer cancel()
	rev, err := w.Rebuild(rctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("the registry could not be read; the configuration in force stays, and it is read again at the next poll",
				"after", why, "err", err)
		}
		return last
	}
	return rev
}

// listen keeps a listener until ctx is done: once it listens, the
// registry is read again, since what changed while nothing listened was
// told to nobody; when it fails, it listens again after a backoff.
func (w *Watcher) listen(ctx context.Context, log *slog.Logger, poke func()) {
	base := or(w.Retry, DefaultRetry)
	wait := base
	for {
		err := w.Listen(ctx, func() { wait = base; poke() }, poke)
		if ctx.Err() != nil {
			return
		}
		log.Warn("the registry's listener was lost; the poll covers for it until it listens again", "in", wait.String(), "err", err)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(2*wait, or(w.RetryMax, DefaultRetryMax))
	}
}

func or(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
