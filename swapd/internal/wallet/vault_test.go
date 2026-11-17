package wallet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
)

func TestCreateRestoreAndWrongPassword(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	v, err := Create(dir, m, "correct-password-ok", keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	eth, xmr := v.Keys.EthAddr.Hex(), v.Keys.XmrAddr

	// The mnemonic must not appear in the clear on disk.
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), strings.Fields(m)[0]+" "+strings.Fields(m)[1]) {
			t.Errorf("mnemonic words appear in the clear in %s", p)
		}
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(v.Keys.EthKey.Hex())) {
			t.Errorf("ETH key appears in the clear in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := OpenVault(dir, "wrong-password-xx"); err == nil {
		t.Fatal("opened with the wrong password")
	}
	v2, err := OpenVault(dir, "correct-password-ok")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Keys.EthAddr.Hex() != eth || v2.Keys.XmrAddr != xmr {
		t.Fatal("reopened wallet has different addresses")
	}

	// Creating again in the same folder must fail.
	if _, err := Create(dir, m, "correct-password-ok", keys.Stagenet); err == nil {
		t.Fatal("created a second wallet in a used folder")
	}

	// Short and empty passwords are refused.
	d2 := t.TempDir()
	if _, err := Create(d2, m, "", keys.Stagenet); err == nil {
		t.Fatal("accepted an empty password")
	}
	if _, err := Create(d2, m, "short", keys.Stagenet); err == nil {
		t.Fatal("accepted a short password")
	}

	// A store opened with a fresh salt in a new dir and the same password
	// is a different store; this just checks store.Open still works here.
	d3 := t.TempDir()
	if _, err := store.Open(d3, "correct-password-ok"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsBadMnemonic(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(dir, "abandon abandon abandon", "correct-password-ok", keys.Stagenet); err == nil {
		t.Fatal("accepted a bad backup")
	}
}
