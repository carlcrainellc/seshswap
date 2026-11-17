package keys

import (
	"errors"
	"math/big"

	"golang.org/x/crypto/sha3"
)

// Network selects the Monero address prefix. Regtest (fakechain) uses mainnet prefixes.
type Network string

const (
	Mainnet  Network = "mainnet"
	Stagenet Network = "stagenet"
	Testnet  Network = "testnet"
	Regtest  Network = "regtest"
)

func (n Network) prefix() (byte, error) {
	switch n {
	case Mainnet, Regtest:
		return 18, nil
	case Stagenet:
		return 24, nil
	case Testnet:
		return 53, nil
	}
	return 0, errors.New("unknown monero network")
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var encSizes = []int{0, 2, 3, 5, 6, 7, 9, 10, 11}

func b58block(b []byte) string {
	n := new(big.Int).SetBytes(b)
	size := encSizes[len(b)]
	res := make([]byte, size)
	for i := range res {
		res[i] = alphabet[0]
	}
	r := new(big.Int)
	f := big.NewInt(58)
	for i := size - 1; i >= 0 && n.Sign() > 0; i-- {
		n.DivMod(n, f, r)
		res[i] = alphabet[r.Int64()]
	}
	return string(res)
}

func moneroB58(data []byte) string {
	s := ""
	for i := 0; i < len(data); i += 8 {
		end := min(i+8, len(data))
		s += b58block(data[i:end])
	}
	return s
}

func keccak(b []byte) []byte { h := sha3.NewLegacyKeccak256(); h.Write(b); return h.Sum(nil) }

// Address returns the standard address for (spend, view) public keys.
func Address(net Network, spend, view Point) (string, error) {
	pf, err := net.prefix()
	if err != nil {
		return "", err
	}
	d := append([]byte{pf}, spend.Bytes()...)
	d = append(d, view.Bytes()...)
	d = append(d, keccak(d)[:4]...)
	return moneroB58(d), nil
}

// Shared describes the jointly controlled Monero address of one swap.
type Shared struct {
	Address  string
	SpendPub Point
	ViewKey  Scalar // private view key, known to both parties
	ViewPub  Point
}

// SharedAddress combines both parties' spend-share public keys and view-key shares.
func SharedAddress(net Network, spendA, spendB Point, viewA, viewB Scalar) (*Shared, error) {
	spend := spendA.Add(spendB)
	view := viewA.Add(viewB)
	vp := view.Public()
	a, err := Address(net, spend, vp)
	if err != nil {
		return nil, err
	}
	return &Shared{Address: a, SpendPub: spend, ViewKey: view, ViewPub: vp}, nil
}
