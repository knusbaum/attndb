package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPollerEmitsTicks(t *testing.T) {
	p := NewPoller(t.TempDir(), 20*time.Millisecond)
	defer p.Close()
	select {
	case batch := <-p.Events():
		if batch != nil {
			t.Fatalf("poller should emit a nil batch (ReconcileAll request), got %v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("poller emitted no tick within 1s")
	}
}

// TestFSEventsWatcher exercises the native macOS watcher end-to-end: a file
// write should surface that path on the Events channel. Skips off darwin.
func TestFSEventsWatcher(t *testing.T) {
	root := t.TempDir()
	w, ok, err := native(root)
	if err != nil {
		t.Fatalf("native watcher: %v", err)
	}
	if !ok {
		t.Skip("no native watcher on this platform")
	}
	defer w.Close()

	// FSEvents needs a moment to arm before it will report changes.
	time.Sleep(300 * time.Millisecond)
	target := filepath.Join(root, "hello.md")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case batch := <-w.Events():
			for _, p := range batch {
				if filepath.Base(p) == "hello.md" {
					return // saw our change
				}
			}
		case <-deadline:
			t.Fatal("no fsevents notification for the written file within 5s")
		}
	}
}
