package exits

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Seller side: signed admissions that the patched exit router checks before
// it accepts a buyer's session (src/auth/exit_lease_admission.hpp, the same
// format as contrib/exit-lease/admission.py). The exit reads the store named
// by SR_EXIT_LEASE_ADMISSIONS and trusts records signed by the key in
// SR_EXIT_LEASE_ISSUER_PUBKEY.

// AdmissionDomain is the signing domain the router checks.
const AdmissionDomain = "srnet-exit-admission/v2"

// Admission is one store record.
type Admission struct {
	Kind   string `json:"kind"`
	Scope  string `json:"scope"`
	V      int    `json:"v"`
	TID    string `json:"tid"`
	Seller string `json:"lock_seller_id"`
	Client string `json:"client_sesh"`
	Exp    int64  `json:"exp"`
	Sig    string `json:"sig"`
}

func admissionMessage(seller, client string, exp int64, tid string) ([]byte, error) {
	for _, f := range []string{seller, client, tid} {
		if strings.ContainsAny(f, "\n") {
			return nil, errors.New("newline in admission field")
		}
	}
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%d\n%s", AdmissionDomain, seller, client, exp, tid)), nil
}

// SignAdmission makes a v2 record.
func SignAdmission(key ed25519.PrivateKey, seller, client string, exp int64, tid string) (*Admission, error) {
	msg, err := admissionMessage(seller, client, exp, tid)
	if err != nil {
		return nil, err
	}
	return &Admission{Kind: "exit_lease_admission", Scope: "exit_lease", V: 2, TID: tid, Seller: seller,
		Client: client, Exp: exp, Sig: hex.EncodeToString(ed25519.Sign(key, msg))}, nil
}

// VerifyAdmission mirrors the router's check; "" = admitted, else a reason.
func VerifyAdmission(a *Admission, pub ed25519.PublicKey, seller, client string, now int64) string {
	if a.Kind != "exit_lease_admission" {
		return "no_match"
	}
	if a.Scope != "exit_lease" || a.V != 2 {
		return "bad_record"
	}
	if a.Seller != seller {
		return "wrong_seller"
	}
	if a.Client != client {
		return "wrong_client"
	}
	msg, err := admissionMessage(a.Seller, a.Client, a.Exp, a.TID)
	sig, err2 := hex.DecodeString(a.Sig)
	if err != nil || err2 != nil || !ed25519.Verify(pub, msg, sig) {
		return "bad_sig"
	}
	if a.Exp <= now {
		return "expired"
	}
	return ""
}

// IssuerKey loads (or with create, makes) a 32-byte hex Ed25519 seed file.
func IssuerKey(path string, create bool) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		seed := make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != 32 {
		return nil, errors.New("issuer key file must hold a 32-byte hex seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// PubHex is the key the exit router is told to trust.
func PubHex(k ed25519.PrivateKey) string { return hex.EncodeToString(k.Public().(ed25519.PublicKey)) }

// Store is admissions.json = {"admissions": {tid: record}}, replaced atomically
// under an flock on <path>.lock (compatible with admission.py).
type Store struct{ Path string }

type storeFile struct {
	Admissions map[string]*Admission `json:"admissions"`
}

func (s *Store) read() (*storeFile, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &storeFile{Admissions: map[string]*Admission{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("admissions store is not valid JSON: %w", err)
	}
	if f.Admissions == nil {
		f.Admissions = map[string]*Admission{}
	}
	return &f, nil
}

func (s *Store) mutate(fn func(*storeFile) error) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	lf, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	f, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".adm-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	_ = os.Chmod(tmp.Name(), 0o640)
	return os.Rename(tmp.Name(), s.Path)
}

// Put adds or replaces a record (same tid = same lease, e.g. after a restart).
func (s *Store) Put(a *Admission) error {
	return s.mutate(func(f *storeFile) error { f.Admissions[a.TID] = a; return nil })
}

// Has reports whether tid is in the store.
func (s *Store) Has(tid string) bool {
	f, err := s.read()
	if err != nil {
		return false
	}
	_, ok := f.Admissions[tid]
	return ok
}

// Prune removes expired records and returns how many.
func (s *Store) Prune(now time.Time) (int, error) {
	n := 0
	err := s.mutate(func(f *storeFile) error {
		for t, a := range f.Admissions {
			if a.Exp <= now.Unix() {
				delete(f.Admissions, t)
				n++
			}
		}
		return nil
	})
	return n, err
}

// Records returns every record.
func (s *Store) Records() ([]*Admission, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]*Admission, 0, len(f.Admissions))
	for _, a := range f.Admissions {
		out = append(out, a)
	}
	return out, nil
}
