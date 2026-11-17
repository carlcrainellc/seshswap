package exits

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const zb32 = "ybndrfg8ejkmcpqxot1uwisza345h769"

// SeshFromPub is the .sesh address of an Ed25519 public key (z-base32, 52 chars + ".sesh").
func SeshFromPub(pub ed25519.PublicKey) string {
	var b strings.Builder
	var acc uint32
	bits := 0
	for _, x := range pub {
		acc = acc<<8 | uint32(x)
		bits += 8
		for bits >= 5 {
			b.WriteByte(zb32[(acc>>(bits-5))&31])
			bits -= 5
		}
	}
	if bits > 0 {
		b.WriteByte(zb32[(acc<<(5-bits))&31])
	}
	return b.String() + ".sesh"
}

// RouterKey loads (or, with create, makes) a Session Router client key file: 128 hex chars,
// the Ed25519 seed followed by the public key, as [network] keyfile= expects. It gives a
// router a .sesh address that stays the same across restarts.
func RouterKey(path string, create bool) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%s is not a router key file", path)
		}
		k := ed25519.PrivateKey(raw)
		if !ed25519.NewKeyFromSeed(k.Seed()).Equal(k) {
			return nil, fmt.Errorf("%s: seed and public key do not match", path)
		}
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, err
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(hex.EncodeToString(k) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}
