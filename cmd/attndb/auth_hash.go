package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kjn/attndb/internal/mcpauth"
	"golang.org/x/term"
)

func runAuthHash(args []string) error {
	fs := flag.NewFlagSet("auth-hash", flag.ContinueOnError)
	out := fs.String("out", "-", "new hash file (0600, refuses overwrite); default stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("auth-hash accepts no password arguments; use the hidden terminal prompt")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("auth-hash requires terminal stdin; passwords cannot be piped or read from a file")
	}
	read := func(prompt string) ([]byte, error) {
		fmt.Fprint(os.Stderr, prompt)
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return value, err
	}
	return generateHashOutput(*out, os.Stdout, read)
}

// read is injected for tests; the real command only uses hidden terminal input.
func generateHashOutput(path string, stdout io.Writer, read func(string) ([]byte, error)) error {
	password, err := read("Password (12..72 bytes): ")
	defer clear(password)
	if err != nil {
		return errors.New("could not read password from terminal")
	}
	confirmation, err := read("Confirm password: ")
	defer clear(confirmation)
	if err != nil {
		return errors.New("could not read confirmation from terminal")
	}
	if !bytes.Equal(password, confirmation) {
		return errors.New("password confirmation does not match")
	}
	hash, err := mcpauth.GeneratePasswordHash(password)
	if err != nil {
		return err
	}
	hash = append(hash, '\n')
	if path == "-" {
		_, err = stdout.Write(hash)
		return err
	}
	// O_EXCL refuses existing files and symlinks; never modify an old secret.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create new password hash file: %w", err)
	}
	_, writeErr := file.Write(hash)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
