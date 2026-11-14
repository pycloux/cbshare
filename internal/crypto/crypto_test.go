package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func TestDeriveKeyIsUniquePerStart(t *testing.T) {
	a, err := DeriveKey("password")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := DeriveKey("password")
	if len(a) != KeySize || bytes.Equal(a, b) {
		t.Fatal("keys must be 32 bytes and differ between starts")
	}
	if _, err := DeriveKey(""); err == nil {
		t.Fatal("empty password must be rejected")
	}
}

func TestKeyEncodingRoundTrip(t *testing.T) {
	key, _ := DeriveKey("pw")
	enc := EncodeKey(key)
	for _, in := range []string{enc, strings.ToLower(enc), strings.ReplaceAll(enc, "-", ""), " " + enc + "\n"} {
		got, err := DecodeKey(in)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("DecodeKey(%q) = %v, %v", in, got, err)
		}
	}
}

func TestDecodeKeyDetectsTypos(t *testing.T) {
	key, _ := DeriveKey("pw")
	enc := []byte(EncodeKey(key))
	if enc[0] == 'A' {
		enc[0] = 'B'
	} else {
		enc[0] = 'A'
	}
	if _, err := DecodeKey(string(enc)); err != ErrInvalidKey {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
	if _, err := DecodeKey("not a key"); err != ErrInvalidKey {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected format %q", h)
	}
	if !VerifyPassword("correct horse", h) {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword("wrong", h) || VerifyPassword("correct horse", "garbage") {
		t.Fatal("invalid password accepted")
	}
}

func TestGeneratePIN(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pin, err := GeneratePIN(6)
		if err != nil {
			t.Fatal(err)
		}
		if len(pin) != 6 || strings.Trim(pin, pinAlphabet) != "" {
			t.Fatalf("bad PIN %q", pin)
		}
		seen[pin] = true
	}
	if len(seen) < 45 {
		t.Fatal("PINs are not random enough")
	}
}

func TestEqualFold(t *testing.T) {
	if !EqualFold("ab3XY9", "AB3xy9") || EqualFold("ABC", "ABD") || EqualFold("ABC", "ABCD") {
		t.Fatal("EqualFold mismatch")
	}
}
