package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// KMS is the part of a key management service the store needs. The key
// versions live behind it and never leave it: the store only sends it data
// keys to wrap and unwrap.
type KMS interface {
	// NewVersion creates a key version for tenant and returns its name.
	NewVersion(ctx context.Context, tenant string) (string, error)
	Wrap(ctx context.Context, version string, dek, aad []byte) ([]byte, error)
	Unwrap(ctx context.Context, version string, wrapped, aad []byte) ([]byte, error)
	// Destroy makes version unusable. Everything it wrapped is gone with it.
	Destroy(ctx context.Context, version string) error
}

// ErrDestroyed is returned by Unwrap when the key version no longer exists.
var ErrDestroyed = errors.New("key version destroyed")

// LocalKMS stands in for Cloud KMS so the demo runs without a cloud account.
// It keeps its keys in a file of their own, never in the database: a key
// stored next to the data it protects ends up in the same backups, and then
// destroying it destroys nothing.
type LocalKMS struct {
	mu   sync.Mutex
	path string
	keys map[string][]byte // version name -> key, nil once destroyed
}

func OpenLocalKMS(path string) (*LocalKMS, error) {
	k := &LocalKMS{path: path, keys: map[string][]byte{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	return k, json.Unmarshal(b, &k.keys)
}

func (k *LocalKMS) NewVersion(_ context.Context, tenant string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	n := 1
	for {
		name := fmt.Sprintf("tenants/%s/versions/%d", tenant, n)
		if _, taken := k.keys[name]; !taken {
			k.keys[name] = key
			return name, k.save()
		}
		n++
	}
}

func (k *LocalKMS) Wrap(_ context.Context, version string, dek, aad []byte) ([]byte, error) {
	key, err := k.key(version)
	if err != nil {
		return nil, err
	}
	return seal(key, dek, aad)
}

func (k *LocalKMS) Unwrap(_ context.Context, version string, wrapped, aad []byte) ([]byte, error) {
	key, err := k.key(version)
	if err != nil {
		return nil, err
	}
	return open(key, wrapped, aad)
}

func (k *LocalKMS) Destroy(_ context.Context, version string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[version] = nil
	return k.save()
}

func (k *LocalKMS) key(version string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, ok := k.keys[version]
	if !ok || key == nil {
		return nil, fmt.Errorf("%s: %w", version, ErrDestroyed)
	}
	return key, nil
}

func (k *LocalKMS) save() error {
	b, err := json.MarshalIndent(k.keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(k.path, b, 0o600)
}

// seal encrypts plaintext with AES-256-GCM under key, binding it to aad.
// The output is nonce || ciphertext || tag.
func seal(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
