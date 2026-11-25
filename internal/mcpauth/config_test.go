package mcpauth

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	s, e := New(Config{})
	if e != nil || s != nil {
		t.Fatal("local development must remain opt-in")
	}
	for _, c := range []Config{{Issuer: "https://example.com"}, {Password: testPassword}, {PasswordFile: "/missing"}, {StateDir: "state"}, {Resource: "https://example.com/mcp"}, {AllowHTTP: true}} {
		if _, e := New(c); e == nil {
			t.Fatalf("partial config accepted: %+v", c)
		}
	}
	for _, issuer := range []string{"http://example.com", "http://localhost:8765", "https://user@example.com", "https://example.com/path", "https://example.com?x=y", "https://example.com#frag", "https://example.com#", "ftp://example.com", "https://"} {
		t.Run(issuer, func(t *testing.T) {
			c := Config{Issuer: issuer, Password: testPassword, StateDir: filepath.Join(t.TempDir(), "auth")}
			if _, e := New(c); e == nil {
				t.Fatal("invalid issuer accepted")
			}
		})
	}
	for _, issuer := range []string{"http://localhost:8765", "http://127.0.0.1:8765", "http://[::1]:8765"} {
		c := Config{Issuer: issuer, Password: testPassword, StateDir: filepath.Join(t.TempDir(), "auth"), AllowHTTP: true}
		s, e := New(c)
		if e != nil {
			t.Fatal(e)
		}
		s.Close()
	}
	c := Config{Issuer: "http://example.com", Password: testPassword, StateDir: filepath.Join(t.TempDir(), "auth"), AllowHTTP: true}
	if _, e = New(c); e == nil {
		t.Fatal("test override permits public HTTP")
	}
	for _, resource := range []string{"https://other.example/mcp", "https://example.com/mcp?q=x", "https://example.com/mcp#", "https://example.com/a/../mcp", "https://example.com/%6dcp", "https://example.com/oauth/token"} {
		c := Config{Issuer: "https://example.com", Resource: resource, Password: testPassword, StateDir: filepath.Join(t.TempDir(), "auth")}
		if _, e := New(c); e == nil {
			t.Fatalf("invalid resource accepted: %s", resource)
		}
	}
}

func TestFlagsAndPasswordFile(t *testing.T) {
	for _, k := range []string{"ATTNDB_AUTH_ISSUER", "ATTNDB_AUTH_RESOURCE", "ATTNDB_AUTH_PASSWORD", "ATTNDB_AUTH_PASSWORD_FILE", "ATTNDB_AUTH_PASSWORD_HASH", "ATTNDB_AUTH_PASSWORD_HASH_FILE", "ATTNDB_AUTH_STATE_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("ATTNDB_AUTH_ISSUER", "https://example.com")
	t.Setenv("ATTNDB_AUTH_STATE_DIR", filepath.Join(t.TempDir(), "auth"))
	secret := filepath.Join(t.TempDir(), "password")
	if e := os.WriteFile(secret, []byte(testPassword+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("ATTNDB_AUTH_PASSWORD_FILE", secret)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := Flags(fs)
	if e := fs.Parse([]string{"-auth-resource", "https://example.com/mcp"}); e != nil {
		t.Fatal(e)
	}
	s, e := New(*cfg)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	cfg.Password = testPassword
	if _, e := New(*cfg); e == nil {
		t.Fatal("ambiguous password sources accepted")
	}
	cfg.Password = ""
	for _, pw := range []string{"", "too short", strings.Repeat("x", 4097)} {
		if e := os.WriteFile(secret, []byte(pw), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := New(*cfg); e == nil {
			t.Fatal("invalid secret file accepted")
		}
	}
	cfg.PasswordFile = filepath.Join(t.TempDir(), "missing")
	if _, e := New(*cfg); e == nil {
		t.Fatal("missing secret file accepted")
	}
}

func TestStateSafetyAndBinding(t *testing.T) {
	s, c := setup(t)
	s.Close()
	changed := c
	changed.Issuer = "https://other.example"
	if _, e := New(changed); e == nil {
		t.Fatal("issuer change accepted existing state")
	}
	changed = c
	changed.Resource = c.Issuer + "/other"
	if _, e := New(changed); e == nil {
		t.Fatal("resource change accepted existing state")
	}
	p := filepath.Join(c.StateDir, "state.json")
	if e := os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := New(c); e == nil {
		t.Fatal("world-readable state accepted")
	}
	os.Chmod(p, 0600)
	if e := os.WriteFile(p, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := New(c); e == nil {
		t.Fatal("corrupt state silently replaced")
	}
	if e := os.Chmod(c.StateDir, 0755); e != nil {
		t.Fatal(e)
	}
	if _, e := New(c); e == nil {
		t.Fatal("public directory accepted")
	}
}
