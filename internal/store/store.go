// Package store persists credential records encrypted at rest (AES-256-GCM).
//
// Records are immutable once stored: refresh builds a new Record and Put()
// replaces the map entry, so concurrent readers never observe a half-mutated
// record.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/Dev-Jahn/ccbroker/internal/creds"
)

type Store struct {
	path string
	gcm  cipher.AEAD
	mu   sync.RWMutex
	data map[string]*creds.Record
}

// Open loads (or initializes) the encrypted store at path. key must be 32 bytes.
func Open(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, gcm: gcm, data: map[string]*creds.Record{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	ns := s.gcm.NonceSize()
	if len(b) < ns {
		return fmt.Errorf("store file too short")
	}
	nonce, ct := b[:ns], b[ns:]
	pt, err := s.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return fmt.Errorf("decrypt store (wrong master key?): %w", err)
	}
	var data map[string]*creds.Record
	if err := json.Unmarshal(pt, &data); err != nil {
		return err
	}
	if data != nil {
		s.data = data
	}
	return nil
}

// saveLocked requires the caller to hold s.mu for writing.
func (s *Store) saveLocked() error {
	pt, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ct := s.gcm.Seal(nonce, nonce, pt, nil)

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".store-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(ct); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// Path returns the store file path.
func (s *Store) Path() string { return s.path }

// Get returns the current (immutable) record for name.
func (s *Store) Get(name string) (*creds.Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.data[name]
	return r, ok
}

// List returns all records.
func (s *Store) List() []*creds.Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*creds.Record, 0, len(s.data))
	for _, r := range s.data {
		out = append(out, r)
	}
	return out
}

// Put replaces the record for r.Name and persists the store.
func (s *Store) Put(r *creds.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[r.Name] = r
	return s.saveLocked()
}

// Delete removes a record.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; !ok {
		return os.ErrNotExist
	}
	delete(s.data, name)
	return s.saveLocked()
}
