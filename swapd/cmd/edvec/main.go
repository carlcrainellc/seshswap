// edvec: test helper. `edvec <decimal-or-0x scalar>` prints the ed25519
// encoding of k*B as a 0x-prefixed little-endian integer (the format used by
// XmrSwapV2). `edvec gen N` prints N random Solidity test vectors.
package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"

	"filippo.io/edwards25519"
)

func le2int(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(r)
}

func int2le32(x *big.Int) []byte {
	be := x.FillBytes(make([]byte, 32))
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		be[i], be[j] = be[j], be[i]
	}
	return be
}

func enc(k *big.Int) *big.Int {
	s, err := new(edwards25519.Scalar).SetCanonicalBytes(int2le32(k))
	if err != nil {
		panic(err)
	}
	return le2int(new(edwards25519.Point).ScalarBaseMult(s).Bytes())
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "gen" {
		var n int
		fmt.Sscan(os.Args[2], &n)
		for i := 0; i < n; i++ {
			var w [64]byte
			if _, err := rand.Read(w[:]); err != nil {
				panic(err)
			}
			s, _ := new(edwards25519.Scalar).SetUniformBytes(w[:])
			k := le2int(s.Bytes())
			fmt.Printf("        v.push(V(0x%064x, 0x%064x));\n", k, enc(k))
		}
		return
	}
	k, ok := new(big.Int).SetString(os.Args[1], 0)
	if !ok {
		panic("bad int")
	}
	// ABI-encode as uint256 for forge ffi
	fmt.Printf("0x%064x", enc(k))
}
