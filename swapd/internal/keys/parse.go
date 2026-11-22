package keys

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
)

// AddressKind is what a Monero address is for.
type AddressKind string

const (
	KindStandard   AddressKind = "standard"
	KindSubaddress AddressKind = "subaddress"
	KindIntegrated AddressKind = "integrated"
)

// address prefixes per network: standard, integrated, subaddress
var prefixes = map[byte]struct {
	net  Network
	kind AddressKind
}{
	18: {Mainnet, KindStandard}, 19: {Mainnet, KindIntegrated}, 42: {Mainnet, KindSubaddress},
	53: {Testnet, KindStandard}, 54: {Testnet, KindIntegrated}, 63: {Testnet, KindSubaddress},
	24: {Stagenet, KindStandard}, 25: {Stagenet, KindIntegrated}, 36: {Stagenet, KindSubaddress},
}

func b58blockDecode(s string, size int) ([]byte, error) {
	n := new(big.Int)
	f := big.NewInt(58)
	for _, c := range []byte(s) {
		i := strings.IndexByte(alphabet, c)
		if i < 0 {
			return nil, errors.New("not a Monero address")
		}
		n.Mul(n, f).Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	if len(b) > size {
		return nil, errors.New("not a Monero address")
	}
	return append(make([]byte, size-len(b)), b...), nil
}

func moneroB58Decode(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i += 11 {
		end := min(i+11, len(s))
		chunk := s[i:end]
		size := -1
		for bl, enc := range encSizes {
			if enc == len(chunk) {
				size = bl
			}
		}
		if size <= 0 {
			return nil, errors.New("not a Monero address")
		}
		b, err := b58blockDecode(chunk, size)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

// ParseAddress checks a Monero address (length, alphabet, checksum, prefix)
// and returns its network and kind.
func ParseAddress(addr string) (Network, AddressKind, error) {
	if len(addr) != 95 && len(addr) != 106 {
		return "", "", errors.New("not a Monero address")
	}
	b, err := moneroB58Decode(addr)
	if err != nil {
		return "", "", err
	}
	if len(b) < 5 || !bytes.Equal(keccak(b[:len(b)-4])[:4], b[len(b)-4:]) {
		return "", "", errors.New("this Monero address has a typo (checksum doesn't match)")
	}
	p, ok := prefixes[b[0]]
	if !ok {
		return "", "", errors.New("not a Monero address")
	}
	want := 69
	if p.kind == KindIntegrated {
		want = 77
	}
	if len(b) != want {
		return "", "", errors.New("not a Monero address")
	}
	if _, err := PointFromBytes(b[1:33]); err != nil {
		return "", "", errors.New("not a Monero address")
	}
	if _, err := PointFromBytes(b[33:65]); err != nil {
		return "", "", errors.New("not a Monero address")
	}
	return p.net, p.kind, nil
}
