// gentable prints the ed25519 constants and the j*B (j=1..15) affine-niels table
// used by contracts/src/Ed25519.sol, computed with math/big from the RFC 8032
// curve definition. Run: go run ./scripts/gentable
package main

import (
	"fmt"
	"math/big"
)

var p, d, bx, by *big.Int

func init() {
	p = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	// d = -121665/121666
	n := new(big.Int).Neg(big.NewInt(121665))
	inv := new(big.Int).ModInverse(big.NewInt(121666), p)
	d = new(big.Int).Mod(new(big.Int).Mul(n, inv), p)
	by = new(big.Int).Mod(new(big.Int).Mul(big.NewInt(4), new(big.Int).ModInverse(big.NewInt(5), p)), p)
	// x = sqrt((y^2-1)/(d y^2+1)), choose even x
	y2 := new(big.Int).Mul(by, by)
	num := new(big.Int).Sub(y2, big.NewInt(1))
	den := new(big.Int).Add(new(big.Int).Mul(d, y2), big.NewInt(1))
	xx := new(big.Int).Mod(new(big.Int).Mul(num, new(big.Int).ModInverse(den, p)), p)
	bx = new(big.Int).ModSqrt(xx, p)
	if bx.Bit(0) == 1 {
		bx.Sub(p, bx)
	}
}

func mod(x *big.Int) *big.Int { return x.Mod(x, p) }

// affine twisted Edwards addition, a = -1
func add(x1, y1, x2, y2 *big.Int) (*big.Int, *big.Int) {
	t := mod(new(big.Int).Mul(d, new(big.Int).Mul(new(big.Int).Mul(x1, x2), new(big.Int).Mul(y1, y2))))
	xn := new(big.Int).Add(new(big.Int).Mul(x1, y2), new(big.Int).Mul(y1, x2))
	yn := new(big.Int).Add(new(big.Int).Mul(y1, y2), new(big.Int).Mul(x1, x2))
	xd := new(big.Int).ModInverse(mod(new(big.Int).Add(big.NewInt(1), t)), p)
	yd := new(big.Int).ModInverse(mod(new(big.Int).Sub(big.NewInt(1), t)), p)
	return mod(xn.Mul(xn, xd)), mod(yn.Mul(yn, yd))
}

func main() {
	fmt.Printf("// p  = 0x%064x\n// 2d = 0x%064x\n// Bx = 0x%064x\n// By = 0x%064x\n", p, mod(new(big.Int).Lsh(d, 1)), bx, by)
	// table 0: j*B; table 1: j*2^128*B
	for tb := 0; tb < 2; tb++ {
		gx, gy := new(big.Int).Set(bx), new(big.Int).Set(by)
		if tb == 1 {
			for i := 0; i < 128; i++ {
				gx, gy = add(gx, gy, gx, gy)
			}
		}
		x, y := new(big.Int).Set(gx), new(big.Int).Set(gy)
		fmt.Printf("// table %d\n", tb)
		for j := 1; j <= 15; j++ {
			ypx := mod(new(big.Int).Add(y, x))
			ymx := mod(new(big.Int).Sub(y, x))
			xy2d := mod(new(big.Int).Mul(new(big.Int).Lsh(d, 1), new(big.Int).Mul(x, y)))
			kw := "else if"
			if j == 1 {
				kw = "if"
			}
			fmt.Printf("        %s (j == %d) return (0x%064x, 0x%064x, 0x%064x);\n", kw, j, ypx, ymx, xy2d)
			x, y = add(x, y, gx, gy)
		}
	}
}
