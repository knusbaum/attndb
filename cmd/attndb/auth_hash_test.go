package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashCommandOutput(t *testing.T) {
	const password = "test-pass-12" // exactly 12 bytes; synthetic fixture
	for _, output := range []string{"stdout", "file"} {
		t.Run(output, func(t *testing.T) {
			path := "-"
			if output == "file" {
				path = filepath.Join(t.TempDir(), "verifier")
			}
			var stdout bytes.Buffer
			var inputs [][]byte
			read := func(string) ([]byte, error) {
				value := []byte(password)
				inputs = append(inputs, value)
				return value, nil
			}
			if err := generateHashOutput(path, &stdout, read); err != nil {
				t.Fatal(err)
			}
			value := stdout.Bytes()
			if output == "file" {
				var err error
				value, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != 0600 || stdout.Len() != 0 {
					t.Fatal("unsafe file output")
				}
				if err := generateHashOutput(path, &stdout, read); err == nil {
					t.Fatal("existing file overwritten")
				}
				again, _ := os.ReadFile(path)
				if !bytes.Equal(again, value) {
					t.Fatal("existing hash changed")
				}
			}
			if bytes.Contains(value, []byte(password)) {
				t.Fatal("plaintext output")
			}
			if err := bcrypt.CompareHashAndPassword(bytes.TrimSuffix(value, []byte("\n")), []byte(password)); err != nil {
				t.Fatal(err)
			}
			for _, input := range inputs {
				for _, b := range input {
					if b != 0 {
						t.Fatal("prompt buffer not cleared")
					}
				}
			}
		})
	}
}

func TestHashCommandRejectsMismatchAndArguments(t *testing.T) {
	var stdout bytes.Buffer
	n := 0
	err := generateHashOutput("-", &stdout, func(string) ([]byte, error) {
		n++
		if n == 1 {
			return []byte("synthetic-first-password"), nil
		}
		return []byte("different-password"), nil
	})
	if err == nil || stdout.Len() != 0 {
		t.Fatal("confirmation mismatch produced output")
	}
	if err := runAuthHash([]string{"never-a-password-argument"}); err == nil {
		t.Fatal("positional password accepted")
	}
	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stdin
	os.Stdin = file
	defer func() { os.Stdin = old }()
	if err := runAuthHash(nil); err == nil {
		t.Fatal("non-terminal input accepted")
	}
}
