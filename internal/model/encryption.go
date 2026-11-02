package model

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"gorm.io/gorm"
)

type encryptionState struct {
	ID    uint `gorm:"primaryKey"`
	Check string
}

// ReservedSecretRef reports whether a legacy reference names CloudPilot's own configuration,
// such as the encryption key or bootstrap admin password, which resources must never read.
func ReservedSecretRef(reference string) bool {
	return strings.HasPrefix(strings.ToUpper(reference), "CLOUDPILOT_")
}

// LookupSecretEnv reads a legacy secret reference, refusing CloudPilot's own configuration variables.
func LookupSecretEnv(reference string) (string, bool) {
	if ReservedSecretRef(reference) {
		return "", false
	}
	value, ok := os.LookupEnv(reference)
	return value, ok && value != ""
}

// ResolveSecret decrypts a stored credential or resolves a legacy environment reference.
func ResolveSecret(encryption cipher.AEAD, purpose, encrypted, reference string) (string, error) {
	if encrypted != "" {
		return Decrypt(encryption, purpose, encrypted)
	}
	if value, ok := LookupSecretEnv(reference); ok {
		return value, nil
	}
	return "", fmt.Errorf("%s credential is not configured", purpose)
}

// Unlock verifies that the supplied encryption key belongs to this database.
func Unlock(db *gorm.DB, encodedKey string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("encryption_key must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	encryption, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	check, err := Encrypt(encryption, "key-check", "cloudpilot")
	if err != nil {
		return nil, err
	}
	state := encryptionState{ID: 1, Check: check}
	if err := db.Where("id = ?", 1).FirstOrCreate(&state).Error; err != nil {
		return nil, err
	}
	if value, err := Decrypt(encryption, "key-check", state.Check); err != nil || value != "cloudpilot" {
		return nil, errors.New("encryption_key does not match this database")
	}
	return encryption, nil
}

// Encrypt seals a value with a random nonce and binds it to its purpose.
func Encrypt(encryption cipher.AEAD, purpose, value string) (string, error) {
	if encryption == nil {
		return "", errors.New("encryption key is unavailable")
	}
	sealed := encryption.Seal(nil, nil, []byte(value), []byte("cloudpilot:"+purpose))
	return "v1:" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt authenticates a stored value before returning its plaintext.
func Decrypt(encryption cipher.AEAD, purpose, value string) (string, error) {
	if encryption == nil {
		return "", errors.New("encryption key is unavailable")
	}
	encoded, ok := strings.CutPrefix(value, "v1:")
	sealed, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if !ok || err != nil {
		return "", errors.New("invalid encrypted credential")
	}
	plaintext, err := encryption.Open(nil, nil, sealed, []byte("cloudpilot:"+purpose))
	if err != nil {
		return "", errors.New("cannot decrypt credential")
	}
	return string(plaintext), nil
}
