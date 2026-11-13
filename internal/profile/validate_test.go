package profile

import (
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		in   string
		want error
	}{
		{"elena.l_1", nil},
		{"abc", nil},
		{strings.Repeat("a", 30), nil},
		{"ab", ErrUsernameLength},
		{strings.Repeat("a", 31), ErrUsernameLength},
		{"elena l", ErrUsernameFormat},
		{"Elena", ErrUsernameFormat},
		{"elena-l", ErrUsernameFormat},
	}
	for _, c := range cases {
		if got := ValidateUsername(c.in); got != c.want {
			t.Errorf("ValidateUsername(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestValidateBio(t *testing.T) {
	if err := ValidateBio(strings.Repeat("x", 160)); err != nil {
		t.Errorf("160 chars: %v", err)
	}
	if err := ValidateBio(strings.Repeat("x", 200)); err != ErrBioTooLong {
		t.Errorf("200 chars: got %v", err)
	}
}

func TestValidateFullName(t *testing.T) {
	if err := ValidateFullName("  E "); err != ErrFullNameLength {
		t.Errorf("1 char: got %v", err)
	}
	if err := ValidateFullName("Elena Larsen"); err != nil {
		t.Errorf("valid: %v", err)
	}
}
