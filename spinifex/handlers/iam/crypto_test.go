package handlers_iam

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
)

func TestGenerateMasterKey(t *testing.T) {
	t.Parallel()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey() error: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(key))
	}

	// Two generated keys should differ
	key2, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey() second call error: %v", err)
	}
	if string(key) == string(key2) {
		t.Fatal("two generated keys should not be identical")
	}
}

func TestSaveAndReadMasterKey(t *testing.T) {
	t.Parallel()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey() error: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	if err := SaveMasterKey(path, key); err != nil {
		t.Fatalf("SaveMasterKey() error: %v", err)
	}

	// Check file permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat master.key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected permissions 0600, got %04o", perm)
	}

	loaded, err := masterkey.ReadShared(path)
	if err != nil {
		t.Fatalf("masterkey.ReadShared() error: %v", err)
	}
	if string(loaded) != string(key) {
		t.Fatal("loaded key does not match saved key")
	}
}

func TestReadMasterKeyWrongSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")

	if err := os.WriteFile(path, []byte("too-short"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := masterkey.ReadShared(path)
	if err == nil {
		t.Fatal("masterkey.ReadShared() should fail for wrong-size key")
	}
}

func TestReadMasterKeyNotFound(t *testing.T) {
	t.Parallel()
	_, err := masterkey.ReadShared("/nonexistent/master.key")
	if err == nil {
		t.Fatal("masterkey.ReadShared() should fail for missing file")
	}
}

func TestSaveMasterKeyWrongSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")

	err := SaveMasterKey(path, []byte("too-short"))
	if err == nil {
		t.Fatal("SaveMasterKey() should fail for wrong-size key")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey() error: %v", err)
	}

	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

	encrypted, err := EncryptSecret(secret, key)
	if err != nil {
		t.Fatalf("EncryptSecret() error: %v", err)
	}

	if encrypted == secret {
		t.Fatal("encrypted output should differ from plaintext")
	}

	decrypted, err := DecryptSecret(encrypted, key)
	if err != nil {
		t.Fatalf("DecryptSecret() error: %v", err)
	}

	if decrypted != secret {
		t.Fatalf("expected %q, got %q", secret, decrypted)
	}
}

func TestEncryptProducesDifferentCiphertexts(t *testing.T) {
	t.Parallel()
	key, _ := GenerateMasterKey()
	secret := "test-secret"

	ct1, _ := EncryptSecret(secret, key)
	ct2, _ := EncryptSecret(secret, key)

	if ct1 == ct2 {
		t.Fatal("encrypting the same plaintext twice should produce different ciphertexts (random nonce)")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	t.Parallel()
	key1, _ := GenerateMasterKey()
	key2, _ := GenerateMasterKey()
	secret := "test-secret"

	encrypted, err := EncryptSecret(secret, key1)
	if err != nil {
		t.Fatalf("EncryptSecret() error: %v", err)
	}

	_, err = DecryptSecret(encrypted, key2)
	if err == nil {
		t.Fatal("DecryptSecret() should fail with wrong key")
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	t.Parallel()
	key, _ := GenerateMasterKey()
	secret := "test-secret"

	encrypted, _ := EncryptSecret(secret, key)

	// Tamper with the ciphertext by flipping a character
	tampered := []byte(encrypted)
	if tampered[len(tampered)-2] == 'A' {
		tampered[len(tampered)-2] = 'B'
	} else {
		tampered[len(tampered)-2] = 'A'
	}

	_, err := DecryptSecret(string(tampered), key)
	if err == nil {
		t.Fatal("DecryptSecret() should fail with tampered ciphertext")
	}
}

func TestDecryptInvalidBase64(t *testing.T) {
	t.Parallel()
	key, _ := GenerateMasterKey()
	_, err := DecryptSecret("not-valid-base64!!!", key)
	if err == nil {
		t.Fatal("DecryptSecret() should fail with invalid base64")
	}
}

func TestDecryptTooShort(t *testing.T) {
	t.Parallel()
	key, _ := GenerateMasterKey()
	// Valid base64 but too short for nonce + ciphertext
	_, err := DecryptSecret("AQID", key)
	if err == nil {
		t.Fatal("DecryptSecret() should fail with too-short ciphertext")
	}
}

func TestLoadBootstrapData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")

	bd := BootstrapData{
		Version:         BootstrapVersion,
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		EncryptedSecret: "dGVzdC1lbmNyeXB0ZWQ=",
		AccountID:       "000000000000",
	}
	data, _ := json.Marshal(bd)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadBootstrapData(path)
	if err != nil {
		t.Fatalf("LoadBootstrapData() error: %v", err)
	}

	if loaded.Version != BootstrapVersion {
		t.Fatalf("Version: expected %q, got %q", BootstrapVersion, loaded.Version)
	}
	if loaded.AccessKeyID != bd.AccessKeyID {
		t.Fatalf("AccessKeyID: expected %q, got %q", bd.AccessKeyID, loaded.AccessKeyID)
	}
	if loaded.EncryptedSecret != bd.EncryptedSecret {
		t.Fatalf("EncryptedSecret mismatch")
	}
	if loaded.AccountID != bd.AccountID {
		t.Fatalf("AccountID: expected %q, got %q", bd.AccountID, loaded.AccountID)
	}
}

func TestLoadBootstrapDataNotFound(t *testing.T) {
	t.Parallel()
	_, err := LoadBootstrapData("/nonexistent/bootstrap.json")
	if err == nil {
		t.Fatal("LoadBootstrapData() should fail for missing file")
	}
}

func TestLoadBootstrapDataInvalidJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")

	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadBootstrapData(path)
	if err == nil {
		t.Fatal("LoadBootstrapData() should fail for invalid JSON")
	}
}

func TestSaveBootstrapData_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")

	data := &BootstrapData{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		EncryptedSecret: "dGVzdC1lbmNyeXB0ZWQ=",
		AccountID:       "000000000000",
	}

	if err := SaveBootstrapData(path, data); err != nil {
		t.Fatalf("SaveBootstrapData() error: %v", err)
	}

	// Verify file permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat bootstrap.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected permissions 0600, got %04o", perm)
	}

	// Verify round-trip
	loaded, err := LoadBootstrapData(path)
	if err != nil {
		t.Fatalf("LoadBootstrapData() error: %v", err)
	}
	if loaded.Version != BootstrapVersion {
		t.Fatalf("Version: expected %q, got %q", BootstrapVersion, loaded.Version)
	}
	if loaded.AccessKeyID != data.AccessKeyID {
		t.Fatalf("AccessKeyID: expected %q, got %q", data.AccessKeyID, loaded.AccessKeyID)
	}
	if loaded.EncryptedSecret != data.EncryptedSecret {
		t.Fatalf("EncryptedSecret: expected %q, got %q", data.EncryptedSecret, loaded.EncryptedSecret)
	}
	if loaded.AccountID != data.AccountID {
		t.Fatalf("AccountID: expected %q, got %q", data.AccountID, loaded.AccountID)
	}
}

func TestBootstrapData_ExtendedRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")

	data := &BootstrapData{
		AccessKeyID:     "AKIASYSTEM1234567890",
		EncryptedSecret: "c3lzdGVtLXNlY3JldA==",
		AccountID:       "000000000000",
		Admin: &AdminBootstrapData{
			AccountID:       "000000000001",
			AccountName:     "spinifex",
			UserName:        "admin",
			AccessKeyID:     "AKIAADMIN12345678901",
			EncryptedSecret: "YWRtaW4tc2VjcmV0",
		},
	}

	if err := SaveBootstrapData(path, data); err != nil {
		t.Fatalf("SaveBootstrapData() error: %v", err)
	}

	loaded, err := LoadBootstrapData(path)
	if err != nil {
		t.Fatalf("LoadBootstrapData() error: %v", err)
	}

	// System fields
	if loaded.AccessKeyID != data.AccessKeyID {
		t.Fatalf("AccessKeyID: expected %q, got %q", data.AccessKeyID, loaded.AccessKeyID)
	}
	if loaded.AccountID != data.AccountID {
		t.Fatalf("AccountID: expected %q, got %q", data.AccountID, loaded.AccountID)
	}

	// Admin fields
	if loaded.Admin == nil {
		t.Fatal("Admin field should not be nil")
	}
	if loaded.Admin.AccountID != "000000000001" {
		t.Fatalf("Admin.AccountID: expected %q, got %q", "000000000001", loaded.Admin.AccountID)
	}
	if loaded.Admin.AccountName != "spinifex" {
		t.Fatalf("Admin.AccountName: expected %q, got %q", "spinifex", loaded.Admin.AccountName)
	}
	if loaded.Admin.UserName != "admin" {
		t.Fatalf("Admin.UserName: expected %q, got %q", "admin", loaded.Admin.UserName)
	}
	if loaded.Admin.AccessKeyID != "AKIAADMIN12345678901" {
		t.Fatalf("Admin.AccessKeyID: expected %q, got %q", "AKIAADMIN12345678901", loaded.Admin.AccessKeyID)
	}
	if loaded.Admin.EncryptedSecret != "YWRtaW4tc2VjcmV0" {
		t.Fatalf("Admin.EncryptedSecret mismatch")
	}
}

func TestBootstrapData_NoAdmin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")

	noAdminJSON := `{"version":"1.0","access_key_id":"AKIAOLD123","encrypted_secret":"b2xk","account_id":"000000000000"}`
	if err := os.WriteFile(path, []byte(noAdminJSON), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadBootstrapData(path)
	if err != nil {
		t.Fatalf("LoadBootstrapData() error: %v", err)
	}

	if loaded.AccessKeyID != "AKIAOLD123" {
		t.Fatalf("AccessKeyID: expected %q, got %q", "AKIAOLD123", loaded.AccessKeyID)
	}
	if loaded.Admin != nil {
		t.Fatal("Admin field should be nil")
	}
}

func TestLoadBootstrapData_MissingVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.json")

	noVersion := `{"access_key_id":"AKIA123","encrypted_secret":"dGVzdA==","account_id":"000000000000"}`
	if err := os.WriteFile(path, []byte(noVersion), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadBootstrapData(path)
	if err == nil {
		t.Fatal("LoadBootstrapData() should fail for missing version")
	}
}

func TestSaveBootstrapData_InvalidPath(t *testing.T) {
	t.Parallel()
	data := &BootstrapData{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		EncryptedSecret: "dGVzdA==",
		AccountID:       "000000000000",
	}
	err := SaveBootstrapData("/nonexistent/dir/bootstrap.json", data)
	if err == nil {
		t.Fatal("SaveBootstrapData() should fail for invalid path")
	}
}
