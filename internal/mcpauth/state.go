package mcpauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const maxClients = 1024
const maxTokens = 8192
const maxPending = 1024
const refreshTTL = 30 * 24 * time.Hour
const clientTTL = 90 * 24 * time.Hour
const accessTTL = time.Hour

type client struct {
	Redirects []string
	Name      string
	Expires   time.Time
}
type token struct {
	Client, Resource, Family string
	Expires                  time.Time
	Used                     bool
}
type diskState struct {
	Version          int
	Issuer, Resource string
	Clients          map[string]client
	Access, Refresh  map[string]token // keys are SHA-256 hashes, never bearer secrets
}

// openState holds an advisory process lock for the lifetime of Server. State
// must live on a local filesystem (not NFS); all writers must use this package.
func (s *Server) openState() error {
	if err := os.MkdirAll(s.cfg.StateDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.cfg.StateDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("auth state directory must be a real private directory (0700)")
	}
	lockPath := filepath.Join(s.cfg.StateDir, "lock")
	if info, err := os.Lstat(lockPath); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return errors.New("unsafe auth lock file")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("auth state already locked: %w", err)
	}
	s.lock = f
	p := filepath.Join(s.cfg.StateDir, "state.json")
	info, err = os.Lstat(p)
	if os.IsNotExist(err) {
		return s.save()
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 32<<20 {
		return errors.New("unsafe auth state file (requires regular file, 0600, <=32MiB)")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	// Decode without new-install defaults: missing fields (or JSON null) must
	// fail validation, never silently acquire this process's issuer/bindings.
	var d diskState
	if err = json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("read auth state: %w", err)
	}
	if d.Version != 1 || d.Issuer != s.cfg.Issuer || d.Resource != s.cfg.Resource || d.Clients == nil || d.Access == nil || d.Refresh == nil || len(d.Clients) > maxClients || len(d.Access)+len(d.Refresh) > maxTokens {
		return errors.New("invalid auth state or issuer/resource mismatch")
	}
	s.disk = d
	s.cleanup(time.Now())
	return nil
}

// save is called under the mutex before exposing a mutation. A storage failure
// poisons this instance: no more bearer access or issuance until restart/repair.
// Atomic rename plus fsync prevents partial JSON and commits refresh rotation.
func (s *Server) save() (err error) {
	defer func() {
		if err != nil {
			s.failed = true
		}
	}()
	b, err := json.Marshal(s.disk)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.cfg.StateDir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.cfg.StateDir, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Server) cleanup(now time.Time) {
	for k, v := range s.disk.Clients {
		if !now.Before(v.Expires) {
			delete(s.disk.Clients, k)
		}
	}
	for _, m := range []map[string]token{s.disk.Access, s.disk.Refresh} {
		for k, v := range m {
			if !now.Before(v.Expires) {
				delete(m, k)
			}
		}
	}
	for k, v := range s.pending {
		if !now.Before(v.Expires) {
			delete(s.pending, k)
		}
	}
	for k, v := range s.codes {
		if !now.Before(v.Expires) {
			delete(s.codes, k)
		}
	}
}

// Close releases the process lock. Stop serving requests before calling Close.
func (s *Server) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	return s.lock.Close()
}
