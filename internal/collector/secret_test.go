package collector

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestPostQuantumSecret(t *testing.T) {
	identity, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := Config{IdentityFile: filepath.Join(dir, "identity.key"), SecretFile: filepath.Join(dir, "secret.age")}
	// Match age-keygen's commented native identity file format.
	key := "# generated identity\n# public key: " + identity.Recipient().String() + "\n" + identity.String() + "\n"
	if err = os.WriteFile(c.IdentityFile, []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	w, err := age.Encrypt(&ciphertext, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, "test-client-secret\n"); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.SecretFile, ciphertext.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	secret, err := c.Secret()
	if err != nil {
		t.Fatal(err)
	}
	if secret != "test-client-secret" {
		t.Fatal("decrypted secret did not match")
	}
}

func TestInvalidIdentityErrorIsSafe(t *testing.T) {
	for _, contents := range []string{"# empty identity file\n", "not-a-key-sensitive-value\n"} {
		file := filepath.Join(t.TempDir(), "identity.key")
		if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		c := Config{IdentityFile: file, SecretFile: "unused.age"}
		_, err := c.Secret()
		if err == nil || !strings.Contains(err.Error(), "expected a native") || strings.Contains(err.Error(), "sensitive-value") {
			t.Fatal("expected actionable, sanitized identity error")
		}
	}
}
