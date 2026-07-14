// Package watch reports filesystem changes under a root directory to the
// reconciler. It defines one interface with two implementations selected per
// platform:
//
//   - a native OS watcher (FSEvents on macOS) that emits the specific changed
//     paths for a low-latency granular reconcile;
//   - a portable polling watcher that emits a nil batch each tick, asking the
//     consumer to run a full ReconcileAll (the only way to notice deletions
//     without OS notifications).
//
// Use Best to get the native watcher where available and the poller otherwise.
package watch

import "time"

// Watcher streams filesystem-change notifications. Each value on Events is a
// batch: a non-empty slice names the changed absolute paths (granular
// reconcile); a nil/empty slice requests a full ReconcileAll. Close stops it.
type Watcher interface {
	Events() <-chan []string
	Close() error
}

// Best returns the lowest-latency watcher available for root: the native OS
// watcher where compiled in, else a poller ticking every interval.
func Best(root string, interval time.Duration) (Watcher, error) {
	if w, ok, err := native(root); err != nil {
		return nil, err
	} else if ok {
		return w, nil
	}
	return NewPoller(root, interval), nil
}

// Poller emits a nil batch every interval so the consumer runs ReconcileAll.
// It watches nothing specific — the full diff is the reconciler's job — so it
// is correct on every platform, just latency-bounded by the interval.
type Poller struct {
	events chan []string
	done   chan struct{}
	tick   time.Duration
}

// NewPoller starts a poller ticking every interval.
func NewPoller(_ string, interval time.Duration) *Poller {
	p := &Poller{
		events: make(chan []string),
		done:   make(chan struct{}),
		tick:   interval,
	}
	go p.loop()
	return p
}

func (p *Poller) loop() {
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			select {
			case p.events <- nil: // nil batch => ReconcileAll
			case <-p.done:
				return
			}
		}
	}
}

func (p *Poller) Events() <-chan []string { return p.events }

func (p *Poller) Close() error {
	close(p.done)
	return nil
}
