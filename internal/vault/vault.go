// Package vault stores secrets (model API keys, bot tokens, webhook secrets)
// encrypted at rest with AES-256-GCM.
//
// The 256-bit master key lives in master.key next to the secrets file. On
// Windows the key file is additionally sealed with DPAPI for the current user;
// elsewhere it relies on 0600 permissions. The goal is that a copied data
// directory, a backup or a stray screenshot of secrets.json does not leak keys;
// it does not protect against code already running as the same OS user.
//
// Secrets never leave the daemon except as environment variables of the
// engine process that needs them; the API only reports whether one is set.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ranjanlive/zpilot/internal/platform"
	"github.com/ranjanlive/zpilot/internal/store"
)

// Vault is safe for concurrent use.
type Vault struct {
	mu   sync.Mutex
	path string
	aead cipher.AEAD
	data map[string]string // ref -> base64(nonce || ciphertext)
}

const keyMagic = "zpilot-key-v1:"

// legacyKeyMagics are headers written before the rename to ZPilot. They are
// still accepted so existing data directories keep working.
var legacyKeyMagics = []string{"zauto-key-v1:", "zautomation-key-v1:"}

// Open loads or creates the vault in dir.
func Open(dir string) (*Vault, error) {
	key, err := loadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v := &Vault{path: filepath.Join(dir, "secrets.json"), aead: aead, data: map[string]string{}}
	b, err := os.ReadFile(v.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &v.data); err != nil {
			return nil, fmt.Errorf("secrets.json is damaged: %w", err)
		}
	}
	return v, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		raw, err := platform.Unprotect(b)
		if err != nil {
			return nil, fmt.Errorf("cannot unseal %s (was the data directory copied from another user or machine?): %w", path, err)
		}
		s, ok := strings.CutPrefix(string(raw), keyMagic)
		for _, m := range legacyKeyMagics {
			if !ok {
				s, ok = strings.CutPrefix(string(raw), m)
			}
		}
		if !ok {
			return nil, fmt.Errorf("%s is not a ZPilot key file", path)
		}
		key, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is damaged", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	sealed, err := platform.Protect([]byte(keyMagic + base64.StdEncoding.EncodeToString(key)))
	if err != nil {
		return nil, err
	}
	if err := store.WriteFileAtomic(path, sealed, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Set stores (or replaces) a secret. An empty value deletes it.
func (v *Vault) Set(ref, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if value == "" {
		delete(v.data, ref)
		return v.save()
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := v.aead.Seal(nonce, nonce, []byte(value), []byte(ref))
	v.data[ref] = base64.StdEncoding.EncodeToString(ct)
	return v.save()
}

// Get returns a secret, or "" if it is not set.
func (v *Vault) Get(ref string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	enc, ok := v.data[ref]
	if !ok || ref == "" {
		return "", nil
	}
	ct, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(ct) < v.aead.NonceSize() {
		return "", fmt.Errorf("secret %s is damaged", ref)
	}
	pt, err := v.aead.Open(nil, ct[:v.aead.NonceSize()], ct[v.aead.NonceSize():], []byte(ref))
	if err != nil {
		return "", fmt.Errorf("secret %s cannot be decrypted with this master key", ref)
	}
	return string(pt), nil
}

// Has reports whether a secret is set.
func (v *Vault) Has(ref string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.data[ref]
	return ok && ref != ""
}

// Delete removes a secret.
func (v *Vault) Delete(ref string) error { return v.Set(ref, "") }

func (v *Vault) save() error { return store.WriteJSONAtomic(v.path, v.data) }
