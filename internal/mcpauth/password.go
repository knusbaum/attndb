package mcpauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const PasswordHashCost = 12

// GeneratePasswordHash returns a randomly salted bcrypt verifier. Enforce the
// byte limit explicitly: bcrypt must never silently equate longer passwords.
func GeneratePasswordHash(password []byte) ([]byte, error) {
	if len(password) < 12 || len(password) > 72 {
		return nil, errors.New("password must contain 12..72 bytes (not characters)")
	}
	return bcrypt.GenerateFromPassword(password, PasswordHashCost)
}

func validatePasswordHash(hash string) error {
	// bcrypt.Cost alone does not validate the complete encoding. Require the
	// supported revisions, exact length, bounded work factor and canonical salt
	// and checksum encodings before accepting a persisted verifier.
	if len(hash) != 60 || !(strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$")) || hash[6] != '$' {
		return errors.New("invalid bcrypt password hash")
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil || cost < 10 || cost > 14 || fmt.Sprintf("%02d", cost) != hash[4:6] {
		return errors.New("bcrypt password hash requires cost 10..14")
	}
	encoding := base64.NewEncoding("./ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789").WithPadding(base64.NoPadding).Strict()
	for _, part := range []string{hash[7:29], hash[29:]} {
		decoded, err := encoding.DecodeString(part)
		if err != nil || encoding.EncodeToString(decoded) != part {
			return errors.New("invalid bcrypt password hash encoding")
		}
	}
	return nil
}

func passwordMatches(hash string, password string) bool {
	return len(password) >= 12 && len(password) <= 72 &&
		bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
