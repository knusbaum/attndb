//go:build darwin

package watch

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsevents"
)

// fsWatcher is the macOS FSEvents-backed Watcher. It is recursive and coalesces
// bursts (the Latency below), so it needs no per-directory registration and
// picks up newly created subdirectories automatically — unlike kqueue.
type fsWatcher struct {
	stream *fsevents.EventStream
	events chan []string
	done   chan struct{}
}

// native starts an FSEvents stream rooted at root.
func native(root string) (Watcher, bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, false, err
	}
	es := &fsevents.EventStream{
		Paths:   []string{abs},
		Latency: 500 * time.Millisecond, // coalesce editor write bursts
		Flags:   fsevents.FileEvents,
	}
	if err := es.Start(); err != nil {
		return nil, false, err
	}
	w := &fsWatcher{stream: es, events: make(chan []string), done: make(chan struct{})}
	go w.loop()
	return w, true, nil
}

func (w *fsWatcher) loop() {
	for {
		select {
		case <-w.done:
			return
		case evs := <-w.stream.Events:
			paths := dedupePaths(evs)
			if len(paths) == 0 {
				continue
			}
			select {
			case w.events <- paths:
			case <-w.done:
				return
			}
		}
	}
}

// dedupePaths turns a batch of FSEvents into a unique set of absolute paths.
// FSEvents may report a path without its leading slash, so normalize it.
func dedupePaths(evs []fsevents.Event) []string {
	seen := make(map[string]bool, len(evs))
	paths := make([]string, 0, len(evs))
	for _, e := range evs {
		p := e.Path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}

func (w *fsWatcher) Events() <-chan []string { return w.events }

func (w *fsWatcher) Close() error {
	close(w.done)
	w.stream.Stop()
	return nil
}
