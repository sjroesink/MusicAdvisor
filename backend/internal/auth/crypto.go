package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Cipher encrypts and decrypts short secrets (refresh tokens, etc) at rest
// using AES-256-GCM. The ciphertext layout is [nonce || sealed]; nonces are
// random per call, so repeated encrypts of the same plaintext produce
// different outputs.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher creates a Cipher from a 32-byte key (AES-256). Any other length
// is a programming error.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("auth.NewCipher: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext. Returns [nonce || ciphertext].
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens a sealed ciphertext produced by Encrypt.
func (c *Cipher) Decrypt(sealed []byte) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("auth.Decrypt: ciphertext too short")
	}
	nonce, body := sealed[:n], sealed[n:]
	return c.aead.Open(nil, nonce, body, nil)
}
