package keys

import "testing"

func randAddr(t *testing.T, net Network) string {
	t.Helper()
	a, _ := RandomScalar()
	b, _ := RandomScalar()
	s, err := Address(net, a.Public(), b.Public())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseAddress(t *testing.T) {
	for _, n := range []Network{Mainnet, Stagenet, Testnet} {
		a := randAddr(t, n)
		got, kind, err := ParseAddress(a)
		if err != nil || got != n || kind != KindStandard {
			t.Fatalf("%s: %v %v %v", n, got, kind, err)
		}
	}
	// The Monero general fund address (published, mainnet).
	if n, k, err := ParseAddress("44AFFq5kSiGBoZ4NMDwYtN18obc8AemS33DBLWs3H7otXft3XjrpDtQGv7SqSsaBYBb98uNbr2VBBEt7f2wfn3RVGQBEP3A"); err != nil || n != Mainnet || k != KindStandard {
		t.Fatalf("real address: %v %v %v", n, k, err)
	}
	// A subaddress on stagenet (prefix 36) built the same way.
	a, _ := RandomScalar()
	b, _ := RandomScalar()
	body := append([]byte{36}, a.Public().Bytes()...)
	body = append(body, b.Public().Bytes()...)
	sub := moneroB58(append(body, keccak(body)[:4]...))
	if n, k, err := ParseAddress(sub); err != nil || n != Stagenet || k != KindSubaddress {
		t.Fatalf("subaddress: %v %v %v", n, k, err)
	}
	// One changed character breaks the checksum.
	good := randAddr(t, Stagenet)
	bad := []byte(good)
	if bad[40] == 'a' {
		bad[40] = 'b'
	} else {
		bad[40] = 'a'
	}
	if _, _, err := ParseAddress(string(bad)); err == nil {
		t.Fatal("typo accepted")
	}
	for _, s := range []string{"", "hello", good[:94], good + "1", "0x02d7033fe94b9918a268f03f472dd44726b59f5b"} {
		if _, _, err := ParseAddress(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
