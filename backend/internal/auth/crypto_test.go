package auth

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestCipher_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("BQD2...fake-spotify-refresh-token-value...xyz")
	sealed, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(sealed, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	opened, err := c.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened = %q, want %q", opened, plaintext)
	}
}

func TestCipher_NonceFreshnessPerEncrypt(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := NewCipher(key)

	a, _ := c.Encrypt([]byte("same-plaintext"))
	b, _ := c.Encrypt([]byte("same-plaintext"))
	if bytes.Equal(a, b) {
		t.Fatal("repeated encrypts of same plaintext produced identical ciphertext; nonce reuse")
	}
}

func TestCipher_TamperDetection(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := NewCipher(key)

	sealed, _ := c.Encrypt([]byte("payload"))
	sealed[len(sealed)-1] ^= 0x01

	if _, err := c.Decrypt(sealed); err == nil {
		t.Fatal("expected auth failure on tampered ciphertext, got nil")
	}
}

func TestCipher_WrongKeyLength(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16)); err == nil {
		t.Fatal("expected error on 16-byte key, got nil")
	}
}
