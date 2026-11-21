// Package store keeps one encrypted, crash-safe file per swap.
//
// Format: "XSV2" | salt(16) | nonce(24) | XChaCha20-Poly1305(JSON).
// Key: Argon2id(passphrase, salt, t=3, m=64 MiB, p=4) derived once per store
// (the salt is kept in store.salt). Writes go to a temp file that is fsynced,
// renamed over the old one, and the directory is fsynced, so a crash leaves
// either the old or the new version, never a torn one.
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

var magic = []byte("XSV2")
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

type Store struct {
	dir string
	key []byte
	mu  sync.Mutex
}

// Open opens (or initializes) a store directory.
func Open(dir, passphrase string) (*Store, error) {
	if passphrase == "" {
		return nil, errors.New("empty passphrase")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sp := filepath.Join(dir, "store.salt")
	salt, err := os.ReadFile(sp)
	if errors.Is(err, os.ErrNotExist) {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		if err := writeAtomic(sp, salt); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, key: argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, 32)}
	// verify passphrase against a canary record
	var c string
	if err := s.Get("canary", &c); errors.Is(err, os.ErrNotExist) {
		if err := s.Put("canary", "ok"); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("wrong passphrase or corrupt store: %w", err)
	}
	return s, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".enc") }

func (s *Store) Put(name string, v any) error {
	if !nameRe.MatchString(name) {
		return errors.New("bad record name")
	}
	pt, err := json.Marshal(v)
	if err != nil {
		return err
	}
	aead, _ := chacha20poly1305.NewX(s.key)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append(append([]byte{}, magic...), nonce...)
	out = aead.Seal(out, nonce, pt, []byte(name)) // record name is authenticated: files can't be swapped
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAtomic(s.path(name), out)
}

func (s *Store) Get(name string, v any) error {
	if !nameRe.MatchString(name) {
		return errors.New("bad record name")
	}
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return err
	}
	aead, _ := chacha20poly1305.NewX(s.key)
	if len(b) < len(magic)+aead.NonceSize()+aead.Overhead() || string(b[:4]) != string(magic) {
		return errors.New("corrupt record")
	}
	nonce := b[4 : 4+aead.NonceSize()]
	pt, err := aead.Open(nil, nonce, b[4+aead.NonceSize():], []byte(name))
	if err != nil {
		return errors.New("decryption failed")
	}
	return json.Unmarshal(pt, v)
}

// List returns record names with the given prefix.
func (s *Store) List(prefix string) ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".enc") && strings.HasPrefix(n, prefix) {
			out = append(out, strings.TrimSuffix(n, ".enc"))
		}
	}
	sort.Strings(out)
	return out, nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}
