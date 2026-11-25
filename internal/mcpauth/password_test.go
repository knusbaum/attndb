package mcpauth

import (
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestPasswordHashValidation(t *testing.T) {
	for _, prefix := range []string{"$2a$", "$2b$"} {
		if err := validatePasswordHash(prefix + testPasswordHash[4:]); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{
		"", "plaintext-is-not-a-hash", testPasswordHash[:59], testPasswordHash + "x",
		"$2y$" + testPasswordHash[4:], "$2a$09$" + testPasswordHash[7:],
		"$2a$15$" + testPasswordHash[7:], "$2a$31$" + testPasswordHash[7:],
		"$2a$xx$" + testPasswordHash[7:], testPasswordHash[:10] + "!" + testPasswordHash[11:],
		testPasswordHash[:28] + "B" + testPasswordHash[29:], // noncanonical salt padding bits
		testPasswordHash[:59] + "B",                         // noncanonical checksum padding bits
		testPasswordHash + "\n", " " + testPasswordHash,
	} {
		c := Config{Issuer: "https://attn.example", StateDir: filepath.Join(t.TempDir(), "state"), PasswordHash: bad}
		if _, err := New(c); err == nil {
			t.Fatal("malformed verifier accepted")
		}
		if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
			t.Fatal("invalid config touched state")
		}
	}
}

func TestPasswordSourceExclusion(t *testing.T) {
	sources := []func(*Config){
		func(c *Config) { c.Password = testPassword },
		func(c *Config) { c.PasswordFile = "/not-read" },
		func(c *Config) { c.PasswordHash = testPasswordHash },
		func(c *Config) { c.PasswordHashFile = "/not-read" },
	}
	for i := range sources {
		for j := i + 1; j < len(sources); j++ {
			c := Config{Issuer: "https://attn.example", StateDir: t.TempDir()}
			sources[i](&c)
			sources[j](&c)
			if _, err := New(c); err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("sources %d/%d not rejected before reading files: %v", i, j, err)
			}
		}
	}
}

func TestHashFileFlagsAndLogin(t *testing.T) {
	for _, key := range []string{"ATTNDB_AUTH_ISSUER", "ATTNDB_AUTH_RESOURCE", "ATTNDB_AUTH_PASSWORD", "ATTNDB_AUTH_PASSWORD_FILE", "ATTNDB_AUTH_PASSWORD_HASH", "ATTNDB_AUTH_PASSWORD_HASH_FILE", "ATTNDB_AUTH_STATE_DIR"} {
		t.Setenv(key, "")
	}
	for _, source := range []string{"file flag", "file environment", "hash environment"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("ATTNDB_AUTH_ISSUER", "https://attn.example")
			t.Setenv("ATTNDB_AUTH_STATE_DIR", filepath.Join(t.TempDir(), "state"))
			path := filepath.Join(t.TempDir(), "hash")
			if err := os.WriteFile(path, []byte(testPasswordHash+"\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var args []string
			switch source {
			case "file flag":
				args = []string{"-auth-password-hash-file", path}
			case "file environment":
				t.Setenv("ATTNDB_AUTH_PASSWORD_HASH_FILE", path)
			case "hash environment":
				t.Setenv("ATTNDB_AUTH_PASSWORD_HASH", testPasswordHash)
			}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			c := Flags(fs)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			s, err := New(*c)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := registerClient(t, s)
			cookie, csrf := loginPage(t, s, authQuery(s, id))
			data(t, request(s, "POST", "/oauth/authorize", url.Values{"csrf": {csrf}, "password": {"wrong-but-long-password"}}.Encode(), "application/x-www-form-urlencoded", cookie), 403)
			tok := tokens(t, s, id)
			if bearer(s, tok["access_token"].(string), "/mcp") != 204 {
				t.Fatal("hash login failed")
			}
			if s.cfg.Password != "" || s.cfg.PasswordHash != "" {
				t.Fatal("config retains password material")
			}
			b, err := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), testPassword) || strings.Contains(string(b), testPasswordHash) {
				t.Fatal("password material persisted in OAuth state")
			}
		})
	}
	for _, value := range []string{"", testPassword, testPasswordHash + "\n\n", strings.Repeat("x", 100)} {
		path := filepath.Join(t.TempDir(), "bad-hash")
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{Issuer: "https://attn.example", StateDir: t.TempDir(), PasswordHashFile: path}); err == nil {
			t.Fatal("invalid hash file accepted")
		}
	}
}

func TestPasswordByteBoundsAndSalt(t *testing.T) {
	for _, pw := range []string{"short", strings.Repeat("x", 73), strings.Repeat("é", 37)} {
		if _, err := GeneratePasswordHash([]byte(pw)); err == nil {
			t.Fatal("password byte limit not enforced")
		}
	}
	pw := strings.Repeat("x", 72)
	hash, err := GeneratePasswordHash([]byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	if !passwordMatches(string(hash), pw) || passwordMatches(string(hash), pw+"x") {
		t.Fatal("bcrypt 72-byte limit bypassed")
	}
	first, err := GeneratePasswordHash([]byte(testPassword))
	if err != nil {
		t.Fatal(err)
	}
	second, err := GeneratePasswordHash([]byte(testPassword))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("salt reused")
	}
	if cost, err := bcrypt.Cost(first); err != nil || cost != 12 {
		t.Fatal("unexpected generation cost")
	}
}
