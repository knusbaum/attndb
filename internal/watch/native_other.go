//go:build !darwin

package watch

// native reports no OS watcher on non-macOS platforms, so Best falls back to
// the poller.
func native(string) (Watcher, bool, error) { return nil, false, nil }
