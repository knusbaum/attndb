package mcpauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestTwelveBytePasswordMinimum(t *testing.T) {
	for _, n := range []int{11, 12} {
		pw := strings.Repeat("x", n)
		generated, err := GeneratePasswordHash([]byte(pw))
		if (err == nil) != (n == 12) {
			t.Fatalf("generation at %d bytes: %v", n, err)
		}
		// Independently create an 11-byte verifier to ensure login itself
		// enforces the minimum, not merely our generation command.
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), 10)
		if err != nil {
			t.Fatal(err)
		}
		if passwordMatches(string(hash), pw) != (n == 12) {
			t.Fatalf("login boundary incorrect at %d bytes", n)
		}
		for _, source := range []string{"raw", "raw file"} {
			c := Config{Issuer: "https://attn.example", StateDir: t.TempDir(), Password: pw}
			if source == "raw file" {
				path := filepath.Join(t.TempDir(), "synthetic-password")
				if err := os.WriteFile(path, []byte(pw), 0600); err != nil {
					t.Fatal(err)
				}
				c.Password, c.PasswordFile = "", path
			}
			if _, err := c.validate(); (err == nil) != (n == 12) {
				t.Fatalf("%s config at %d bytes: %v", source, n, err)
			}
		}
		if n == 12 {
			if !passwordMatches(string(generated), pw) {
				t.Fatal("generated 12-byte password fails login")
			}
			c := Config{Issuer: "https://attn.example", StateDir: t.TempDir(), PasswordHash: string(generated)}
			if _, err := c.validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
