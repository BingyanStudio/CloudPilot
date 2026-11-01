package model

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

// TestEncryption authenticates ciphertext, its purpose and the database's persistent key.
func TestEncryption(t *testing.T) {
	db, err := Open("sqlite", filepath.Join(t.TempDir(), "encrypted.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	encryption, err := Unlock(db, key)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Encrypt(encryption, "Worker", "private-api-key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encrypt(encryption, "Worker", "private-api-key")
	if err != nil || first == second || strings.Contains(first, "private-api-key") {
		t.Fatal("encryption must use a fresh nonce and hide the plaintext")
	}
	restarted, err := Unlock(db, key)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := Decrypt(restarted, "Worker", first); err != nil || value != "private-api-key" {
		t.Fatal("same key could not decrypt after restart", err)
	}
	if _, err := Decrypt(restarted, "Runner", first); err == nil {
		t.Fatal("ciphertext was accepted for a different purpose")
	}
	tampered, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(first, "v1:"))
	if err != nil {
		t.Fatal(err)
	}
	tampered[len(tampered)-1] ^= 1
	for _, value := range []string{"plaintext", "v2:invalid", "v1:", "v1:" + base64.StdEncoding.EncodeToString(tampered)} {
		if _, err := Decrypt(restarted, "Worker", value); err == nil {
			t.Fatal("accepted an invalid or tampered credential")
		}
	}
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short")), base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))} {
		if _, err := Unlock(db, value); err == nil {
			t.Fatal("accepted an invalid or different encryption key")
		}
	}
	if _, err := Unlock(db, key); err != nil {
		t.Fatal("wrong-key attempt changed the stored key check", err)
	}
	if _, err := Encrypt(nil, "Worker", "private-api-key"); err == nil {
		t.Fatal("saved a credential without an encryption key")
	}
}
