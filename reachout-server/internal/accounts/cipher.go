// Package accounts manages connected LinkedIn accounts: the connect flow, status,
// encrypted identification tokens and per-account schedule settings.
package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const cipherPrefix = "v1:"

// Cipher encrypts secrets (identification tokens) with AES-256-GCM before they are
// stored. Each value is bound to a context string (its account row) as additional
// authenticated data, so a ciphertext copied into another row fails to decrypt.
type Cipher struct{ aead cipher.AEAD }

func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
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

// Encrypt returns "v1:" + base64(nonce || ciphertext).
func (c *Cipher) Encrypt(plaintext, bind string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte(bind))
	return cipherPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

var errDecrypt = errors.New("accounts: cannot decrypt value")

// Decrypt reverses Encrypt. The same bind string must be supplied.
func (c *Cipher) Decrypt(value, bind string) (string, error) {
	if !strings.HasPrefix(value, cipherPrefix) {
		return "", errDecrypt
	}
	raw, err := base64.RawStdEncoding.DecodeString(value[len(cipherPrefix):])
	if err != nil || len(raw) < c.aead.NonceSize() {
		return "", errDecrypt
	}
	n := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, raw[:n], raw[n:], []byte(bind))
	if err != nil {
		return "", errDecrypt
	}
	return string(plain), nil
}
