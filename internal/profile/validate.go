// Package profile holds profile field rules shared by the API handlers.
// They mirror the CHECK constraints on public.profiles, so the API can answer
// 400 with a field message before the database rejects the write.
package profile

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9_.]+$`)

// Field errors, worded for display.
var (
	ErrUsernameLength = errors.New("Username must be 3–30 characters.")
	ErrUsernameFormat = errors.New("Use only lowercase letters, numbers, dots and underscores, with no spaces.")
	ErrBioTooLong     = errors.New("Bio must be 160 characters or fewer.")
	ErrFullNameLength = errors.New("Full name must be 2–80 characters.")
)

// ValidateUsername checks length (3–30) and the [a-z0-9_.] alphabet.
func ValidateUsername(u string) error {
	n := utf8.RuneCountInString(u)
	if strings.ContainsAny(u, " \t\n") {
		return ErrUsernameFormat
	}
	if n < 3 || n > 30 {
		return ErrUsernameLength
	}
	if !usernamePattern.MatchString(u) {
		return ErrUsernameFormat
	}
	return nil
}

// ValidateBio allows at most 160 characters.
func ValidateBio(bio string) error {
	if utf8.RuneCountInString(bio) > 160 {
		return ErrBioTooLong
	}
	return nil
}

// ValidateFullName requires 2–80 characters after trimming.
func ValidateFullName(name string) error {
	n := utf8.RuneCountInString(strings.TrimSpace(name))
	if n < 2 || n > 80 {
		return ErrFullNameLength
	}
	return nil
}
