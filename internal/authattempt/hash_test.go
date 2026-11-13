package authattempt

import "testing"

func TestNormalizeIdentifier(t *testing.T) {
	cases := map[string]string{
		"  Asha@Example.COM ": "asha@example.com",
		"9876543210":          "+919876543210",
		"+91 98765-43210":     "+919876543210",
		"919876543210":        "+919876543210",
		"09876543210":         "+919876543210",
	}
	for in, want := range cases {
		got, err := NormalizeIdentifier(in)
		if err != nil || got != want {
			t.Errorf("NormalizeIdentifier(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"98765", "98765432101234", "98765x3210", "", "9+876543210"} {
		if _, err := NormalizeIdentifier(bad); err != ErrInvalidPhone {
			t.Errorf("NormalizeIdentifier(%q) err = %v; want ErrInvalidPhone", bad, err)
		}
	}
}

func TestIdentifierHashIsStableAndHex(t *testing.T) {
	a, _ := IdentifierHash("Asha@Example.com")
	b, _ := IdentifierHash("asha@example.com ")
	if a != b {
		t.Fatalf("hashes differ for the same normalised email")
	}
	if len(a) != 64 {
		t.Fatalf("hash length %d, want 64", len(a))
	}
	p1, _ := IdentifierHash("9876543210")
	p2, _ := IdentifierHash("+91 98765 43210")
	if p1 != p2 || p1 == a {
		t.Fatalf("phone hashes not normalised")
	}
}
