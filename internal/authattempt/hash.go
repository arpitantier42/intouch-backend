// Package authattempt computes the hashes stored in auth_attempts and
// known_devices. Raw emails, phone numbers and device fingerprints are never
// written to the database; only these hashes are.
package authattempt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrInvalidPhone is returned for a phone identifier that is not an Indian
// mobile number with 10 digits.
var ErrInvalidPhone = errors.New("phone number must have 10 digits")

// NormalizeIdentifier returns the canonical form of an OTP or sign-in
// identifier: a trimmed, lower-cased email, or an Indian mobile number as
// +91 followed by its 10 digits (accepting spaces, dashes and a +91/91/0
// prefix).
func NormalizeIdentifier(identifier string) (string, error) {
	s := strings.TrimSpace(identifier)
	if strings.Contains(s, "@") {
		return strings.ToLower(s), nil
	}
	var digits strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || (r == '+' && digits.Len() == 0):
		default:
			return "", ErrInvalidPhone
		}
	}
	d := digits.String()
	switch {
	case len(d) == 12 && strings.HasPrefix(d, "91"):
		d = d[2:]
	case len(d) == 11 && strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	if len(d) != 10 {
		return "", ErrInvalidPhone
	}
	return "+91" + d, nil
}

// IdentifierHash is the identifier_hash column value: hex sha256 of the
// normalised identifier.
func IdentifierHash(identifier string) (string, error) {
	n, err := NormalizeIdentifier(identifier)
	if err != nil {
		return "", err
	}
	return Hash(n), nil
}

// Hash returns the lower-case hex sha256 of s (64 characters), the format
// the identifier_hash and device_hash columns require.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
