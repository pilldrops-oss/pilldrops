package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// encPrefix marks a sealed value so open can distinguish it from legacy plaintext.
const encPrefix = "enc:v1:"

// crypter encrypts small secret strings at rest with AES-256-GCM. A nil
// *crypter means encryption is disabled and values pass through unchanged.
type crypter struct{ aead cipher.AEAD }

// newCrypter builds a crypter from a 64-char hex key; an empty key returns (nil, nil).
func newCrypter(keyHex string) (*crypter, error) {
	if keyHex == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("SANDBOXD_SECRET_KEY: not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("SANDBOXD_SECRET_KEY must be 32 bytes (64 hex chars)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &crypter{aead: aead}, nil
}

// seal encrypts s as "enc:v1:<base64(nonce||ciphertext)>".
func (c *crypter) seal(s string) (string, error) {
	if c == nil {
		return s, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, []byte(s), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

// open reverses seal. A value without encPrefix is returned as-is.
func (c *crypter) open(s string) (string, error) {
	if !strings.HasPrefix(s, encPrefix) {
		return s, nil
	}
	if c == nil {
		return "", errors.New("value is encrypted but SANDBOXD_SECRET_KEY is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, encPrefix))
	if err != nil {
		return "", err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
