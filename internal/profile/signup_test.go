package profile

import (
	"strings"
	"testing"
)

func TestSignUpValidate(t *testing.T) {
	ok := SignUp{FullName: "Asha Rao", Email: "asha@example.com", Password: "Secret123", ConfirmPassword: "Secret123"}
	if errs := ok.Validate(); len(errs) != 0 {
		t.Fatalf("valid form rejected: %v", errs)
	}
	cases := []struct {
		mut         func(*SignUp)
		field, want string
	}{
		{func(s *SignUp) { s.FullName = "  A " }, "full_name", MsgName},
		{func(s *SignUp) { s.FullName = strings.Repeat("a", 81) }, "full_name", MsgName},
		{func(s *SignUp) { s.Email = strings.Repeat("a", 246) + "@example.com" }, "email", MsgEmail},
		{func(s *SignUp) { s.Email = "Asha <a@b.co>" }, "email", MsgEmail},
		{func(s *SignUp) { s.Password, s.ConfirmPassword = "1234567", "1234567" }, "password", MsgPasswordShort},
		{func(s *SignUp) { s.Password = strings.Repeat("x", 73); s.ConfirmPassword = s.Password }, "password", MsgPasswordLong},
		{func(s *SignUp) { s.ConfirmPassword = "Secret124" }, "confirm_password", MsgPasswordMatch},
	}
	for i, c := range cases {
		s := ok
		c.mut(&s)
		if got := s.Validate()[c.field]; got != c.want {
			t.Errorf("case %d: %s = %q, want %q", i, c.field, got, c.want)
		}
	}
}

func TestValidateNewPassword(t *testing.T) {
	for pw, want := range map[string]string{
		"password1":               MsgPasswordRules,
		"PASSWORD1":               MsgPasswordRules,
		"Password":                MsgPasswordRules,
		"Pass1":                   MsgPasswordShort,
		strings.Repeat("Aa1", 25): MsgPasswordLong,
		"Password1":               "",
	} {
		if got := ValidateNewPassword(pw); got != want {
			t.Errorf("%q: got %q, want %q", pw, got, want)
		}
	}
}
