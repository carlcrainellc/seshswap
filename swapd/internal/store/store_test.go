package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripAndWrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	type rec struct{ Secret string }
	if err := s.Put("swap-1", rec{"top secret scalar"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "swap-1.enc"))
	if strings.Contains(string(raw), "top secret") {
		t.Fatal("plaintext on disk")
	}
	var r rec
	if err := s.Get("swap-1", &r); err != nil || r.Secret != "top secret scalar" {
		t.Fatal("roundtrip", err)
	}
	if _, err := Open(dir, "wrong"); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	s2, err := Open(dir, "correct horse") // reopen after "restart"
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := s2.List("swap-"); len(names) != 1 {
		t.Fatal("list", names)
	}
}

func TestTamperAndSwapDetected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "pw")
	_ = s.Put("a", "A")
	_ = s.Put("b", "B")
	// moving b's file over a must fail authentication (name is associated data)
	b, _ := os.ReadFile(filepath.Join(dir, "b.enc"))
	_ = os.WriteFile(filepath.Join(dir, "a.enc"), b, 0o600)
	var v string
	if err := s.Get("a", &v); err == nil {
		t.Fatal("swapped file accepted")
	}
	b[len(b)-1] ^= 1
	_ = os.WriteFile(filepath.Join(dir, "b.enc"), b, 0o600)
	if err := s.Get("b", &v); err == nil {
		t.Fatal("tampered file accepted")
	}
	if err := s.Put("../evil", "x"); err == nil {
		t.Fatal("path traversal name accepted")
	}
}
