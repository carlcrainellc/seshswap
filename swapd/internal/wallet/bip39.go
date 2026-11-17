// Package wallet is the standalone swap wallet: one BIP-39 seed, encrypted
// at rest, a local page on 127.0.0.1, and the existing swapd engine behind it.
package wallet

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// NewMnemonic returns a new 24-word BIP-39 English backup (256 bits of entropy).
func NewMnemonic() (string, error) {
	ent := make([]byte, 32)
	if _, err := rand.Read(ent); err != nil {
		return "", err
	}
	return EncodeMnemonic(ent)
}

// EncodeMnemonic turns 16, 20, 24, 28 or 32 bytes of entropy into a BIP-39 phrase.
func EncodeMnemonic(entropy []byte) (string, error) {
	if len(entropy) < 16 || len(entropy) > 32 || len(entropy)%4 != 0 {
		return "", errors.New("entropy must be 16–32 bytes, a multiple of 4")
	}
	csBits := len(entropy) / 4 // 1 checksum bit per 32 bits of entropy
	h := sha256.Sum256(entropy)
	// entropy bits followed by the first csBits of the hash
	bits := append([]byte{}, entropy...)
	bits = append(bits, h[0]) // enough for up to 8 checksum bits (24-word)
	nwords := (len(entropy)*8 + csBits) / 11
	words := make([]string, nwords)
	for i := 0; i < nwords; i++ {
		words[i] = bip39English[bit11(bits, i)]
	}
	return strings.Join(words, " "), nil
}

func bit11(b []byte, i int) int {
	// 11 bits starting at bit i*11 (MSB of b[0] is bit 0)
	start := i * 11
	var v int
	for j := 0; j < 11; j++ {
		bit := start + j
		if b[bit/8]&(1<<uint(7-bit%8)) != 0 {
			v |= 1 << uint(10-j)
		}
	}
	return v
}

// ParseMnemonic checks a BIP-39 English phrase and returns its entropy.
func ParseMnemonic(phrase string) ([]byte, error) {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(phrase)))
	n := len(parts)
	if n != 12 && n != 15 && n != 18 && n != 21 && n != 24 {
		return nil, errors.New("backup must be 12, 15, 18, 21 or 24 words")
	}
	idx := make([]int, n)
	for i, w := range parts {
		j, ok := bip39Index(w)
		if !ok {
			return nil, fmt.Errorf("word %d is not in the backup word list", i+1)
		}
		idx[i] = j
	}
	// n*11 bits, packed MSB first, then split into entropy and checksum.
	total := n * 11
	csBits := n / 3 // 12 words → 4 bits, 24 words → 8 bits
	entLen := (total - csBits) / 8
	buf := make([]byte, (total+7)/8)
	for i, v := range idx {
		for j := 0; j < 11; j++ {
			if v&(1<<uint(10-j)) != 0 {
				bit := i*11 + j
				buf[bit/8] |= 1 << uint(7-bit%8)
			}
		}
	}
	entropy := buf[:entLen]
	h := sha256.Sum256(entropy)
	for i := 0; i < csBits; i++ {
		bit := entLen*8 + i
		want := buf[bit/8]&(1<<uint(7-bit%8)) != 0
		got := h[i/8]&(1<<uint(7-i%8)) != 0
		if want != got {
			return nil, errors.New("backup checksum does not match: a word is wrong or out of order")
		}
	}
	out := make([]byte, entLen)
	copy(out, entropy)
	return out, nil
}

func bip39Index(w string) (int, bool) {
	// The list is sorted; binary search.
	lo, hi := 0, 2048
	for lo < hi {
		mid := (lo + hi) / 2
		if bip39English[mid] < w {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < 2048 && bip39English[lo] == w {
		return lo, true
	}
	return 0, false
}

// Seed is the BIP-39 PBKDF2 seed (64 bytes) from a checked mnemonic.
// passphrase is the optional BIP-39 passphrase, not the wallet password;
// the wallet always passes "" (no BIP-39 passphrase).
func Seed(mnemonic, passphrase string) ([]byte, error) {
	if _, err := ParseMnemonic(mnemonic); err != nil {
		return nil, err
	}
	norm := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(mnemonic))), " ")
	return pbkdf2.Key([]byte(norm), []byte("mnemonic"+passphrase), 2048, 64, sha512.New), nil
}
