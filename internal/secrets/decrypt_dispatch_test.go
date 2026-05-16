package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// TestDecryptAgeWinsOverSops locks in the rule that an age armor header
// at the start of the file ALWAYS routes to the age path — even when
// the file's extension hints at sops. This protects old whole-file age
// yaml/json files (encrypted before the dual-backend release) from
// being mistakenly handed to sops's parser.
func TestDecryptAgeWinsOverSops(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(keyPath, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AXUP_AGE_KEY", keyPath)
	KeyPaths = nil

	plain := []byte("hello from the old age path\n")
	cipher := encryptAgeArmor(t, plain, id.Recipient())

	if !LooksAgeEncrypted(cipher) {
		t.Fatal("setup: armor output should match LooksAgeEncrypted")
	}
	got, err := Decrypt(cipher)
	if err != nil {
		t.Fatalf("Decrypt() failed on whole-file age input: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch:\nwant %q\ngot  %q", plain, got)
	}
}

// TestDecryptPassthrough confirms that non-encrypted bytes return unchanged.
func TestDecryptPassthrough(t *testing.T) {
	plain := []byte("just a plaintext file\n")
	got, err := Decrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("passthrough mismatch:\nwant %q\ngot  %q", plain, got)
	}
}

func encryptAgeArmor(t *testing.T, data []byte, r age.Recipient) []byte {
	t.Helper()
	var buf bytes.Buffer
	armorW := armor.NewWriter(&buf)
	w, err := age.Encrypt(armorW, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := armorW.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
