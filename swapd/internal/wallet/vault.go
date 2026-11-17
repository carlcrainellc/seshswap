package wallet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
)

const vaultRecord = "vault"

// File is the encrypted-at-rest wallet record (seed and derived keys).
// It is only ever written through store.Store (Argon2id + XChaCha20-Poly1305).
type File struct {
	Mnemonic string        `json:"mnemonic"`
	EthKey   string        `json:"ethKey"`
	XmrSpend string        `json:"xmrSpend"`
	XmrView  string        `json:"xmrView"`
	XmrAddr  string        `json:"xmrAddr"`
	XmrNet   keys.Network  `json:"xmrNet"`
	EthAddr  string        `json:"ethAddr"`
}

// Vault is an unlocked wallet: the store, the keys, and a lock for callers.
type Vault struct {
	Dir   string
	Store *store.Store
	Keys  *Keys
	File  File
	mu    sync.Mutex
}

// Exists reports whether dir already has a wallet (a vault record).
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, vaultRecord+".enc"))
	return err == nil
}

// Create writes a new wallet from a mnemonic (already checked) and password.
func Create(dir, mnemonic, password string, net keys.Network) (*Vault, error) {
	if password == "" {
		return nil, errors.New("password cannot be empty")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	if Exists(dir) {
		return nil, errors.New("a wallet already exists in this folder")
	}
	seed, err := Seed(mnemonic, "")
	if err != nil {
		return nil, err
	}
	k, err := Derive(seed, net)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(dir, password)
	if err != nil {
		return nil, err
	}
	f := File{
		Mnemonic: mnemonic,
		EthKey:   k.EthKey.Hex(),
		XmrSpend: k.XmrSpend.Hex(),
		XmrView:  k.XmrView.Hex(),
		XmrAddr:  k.XmrAddr,
		XmrNet:   net,
		EthAddr:  k.EthAddr.Hex(),
	}
	if err := st.Put(vaultRecord, f); err != nil {
		return nil, err
	}
	return &Vault{Dir: dir, Store: st, Keys: k, File: f}, nil
}

// OpenVault decrypts an existing wallet with the password.
func OpenVault(dir, password string) (*Vault, error) {
	if !Exists(dir) {
		return nil, errors.New("no wallet in this folder")
	}
	st, err := store.Open(dir, password)
	if err != nil {
		return nil, err
	}
	var f File
	if err := st.Get(vaultRecord, &f); err != nil {
		return nil, fmt.Errorf("wallet record: %w", err)
	}
	seed, err := Seed(f.Mnemonic, "")
	if err != nil {
		return nil, err
	}
	k, err := Derive(seed, f.XmrNet)
	if err != nil {
		return nil, err
	}
	if k.EthAddr.Hex() != f.EthAddr || k.XmrAddr != f.XmrAddr {
		return nil, errors.New("stored addresses do not match the backup; the file may be corrupt")
	}
	return &Vault{Dir: dir, Store: st, Keys: k, File: f}, nil
}

// StorePath is the default data directory (mode 0700 once created).
func StorePath(override string) string {
	if override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".swapwallet"
	}
	return filepath.Join(home, ".swapwallet")
}
