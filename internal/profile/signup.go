package profile

import (
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Client-facing validation messages, identical to the app's form errors.
const (
	MsgName          = "Enter your name"
	MsgEmail         = "Enter a valid email"
	MsgPasswordShort = "At least 8 characters"
	MsgPasswordLong  = "Too long"
	MsgPasswordMatch = "Passwords do not match"
	MsgPasswordRules = "Use upper and lower case letters and a digit"
)

// SignUp is the sign-up form.
type SignUp struct {
	FullName        string `json:"full_name"`
	Email           string `json:"email"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirm_password"`
}

// ValidateEmail accepts a single plain address of at most 255 characters.
func ValidateEmail(email string) bool {
	e := strings.TrimSpace(email)
	if e == "" || len(e) > 255 || strings.ContainsAny(e, " <>") {
		return false
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e {
		return false
	}
	at := strings.LastIndex(e, "@")
	return at > 0 && strings.Contains(e[at+1:], ".")
}

// PasswordLength returns "" or the message for a password outside 8–72
// characters (72 bytes is the bcrypt limit Supabase Auth applies).
func PasswordLength(pw string) string {
	switch {
	case utf8.RuneCountInString(pw) < 8:
		return MsgPasswordShort
	case len(pw) > 72:
		return MsgPasswordLong
	}
	return ""
}

// ValidateNewPassword is the policy for a new password set by change or
// reset: 8–72 characters with an upper-case letter, a lower-case letter and
// a digit. Returns "" when the password is acceptable.
func ValidateNewPassword(pw string) string {
	if m := PasswordLength(pw); m != "" {
		return m
	}
	var up, low, digit bool
	for _, r := range pw {
		up = up || unicode.IsUpper(r)
		low = low || unicode.IsLower(r)
		digit = digit || unicode.IsDigit(r)
	}
	if !up || !low || !digit {
		return MsgPasswordRules
	}
	return ""
}

// Validate returns field → message for every invalid sign-up field
// (empty when the form is valid).
func (s SignUp) Validate() map[string]string {
	errs := map[string]string{}
	if ValidateFullName(s.FullName) != nil {
		errs["full_name"] = MsgName
	}
	if !ValidateEmail(s.Email) {
		errs["email"] = MsgEmail
	}
	if m := PasswordLength(s.Password); m != "" {
		errs["password"] = m
	}
	if s.ConfirmPassword != s.Password {
		errs["confirm_password"] = MsgPasswordMatch
	}
	return errs
}
