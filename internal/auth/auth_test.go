package auth

import (
	"encoding/hex"
	"testing"
)

func TestDeriveKeyRFC6070Vector(t *testing.T) {
	// RFC 6070 PBKDF2-HMAC-SHA256 Test Vector:
	// P = "password"
	// S = "salt"
	// c = 1
	// dkLen = 32
	// Expected: 120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b
	p := "password"
	s := []byte("salt")
	dk := DeriveKey(p, s, 1, 32)
	got := hex.EncodeToString(dk)
	want := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("DeriveKey vector mismatch:\ngot:  %s\nwant: %s", got, want)
	}

	// Vector 2: c = 2
	// Expected: ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43
	dk2 := DeriveKey(p, s, 2, 32)
	got2 := hex.EncodeToString(dk2)
	want2 := "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"
	if got2 != want2 {
		t.Fatalf("DeriveKey vector 2 mismatch:\ngot:  %s\nwant: %s", got2, want2)
	}
}

func TestHashAndVerifyPassphrase(t *testing.T) {
	pass := "my-secret-vault-passphrase-2026!"
	hashB64, saltB64, err := HashPassphrase(pass)
	if err != nil {
		t.Fatalf("unexpected error hashing passphrase: %v", err)
	}
	if hashB64 == "" || saltB64 == "" {
		t.Fatalf("empty hash or salt returned")
	}

	// Verification with exact passphrase must succeed
	if !VerifyPassphrase(pass, hashB64, saltB64) {
		t.Fatalf("failed to verify correct passphrase")
	}

	// Verification with wrong passphrase must fail
	if VerifyPassphrase("wrong-password", hashB64, saltB64) {
		t.Fatalf("expected wrong passphrase to fail verification")
	}

	// Verification with empty passphrase or corrupt hash must fail
	if VerifyPassphrase("", hashB64, saltB64) {
		t.Fatalf("expected empty passphrase to fail verification")
	}
	if VerifyPassphrase(pass, "invalid-base64-hash!", saltB64) {
		t.Fatalf("expected corrupt hash to fail verification")
	}
}

func TestGenerateSessionToken(t *testing.T) {
	tok1, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	tok2, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	if len(tok1) != SessionTokenLen*2 { // hex encoded
		t.Fatalf("unexpected token length: got %d, want %d", len(tok1), SessionTokenLen*2)
	}
	if tok1 == tok2 {
		t.Fatalf("expected distinct random session tokens, got identical")
	}
}
