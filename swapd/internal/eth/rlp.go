// Package eth is a minimal, dependency-light Ethereum client for XmrSwapV2:
// JSON-RPC, EIP-1559 transaction signing, ABI encoding of the few calls we
// need, event decoding, and EIP-712 claim signatures. Written from the
// Ethereum Yellow Paper / EIPs (RLP, EIP-155/1559/712); no go-ethereum code.
package eth

import "math/big"

// rlpBytes encodes a byte string.
func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpLen(len(b), 0x80), b...)
}

func rlpLen(n int, off byte) []byte {
	if n < 56 {
		return []byte{off + byte(n)}
	}
	var lb []byte
	for x := n; x > 0; x >>= 8 {
		lb = append([]byte{byte(x)}, lb...)
	}
	return append([]byte{off + 55 + byte(len(lb))}, lb...)
}

// rlpInt encodes a non-negative integer (big-endian, no leading zeros).
func rlpInt(x *big.Int) []byte {
	if x == nil || x.Sign() == 0 {
		return []byte{0x80}
	}
	return rlpBytes(x.Bytes())
}

func rlpUint(x uint64) []byte { return rlpInt(new(big.Int).SetUint64(x)) }

// rlpList wraps already-encoded items.
func rlpList(items ...[]byte) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, it...)
	}
	return append(rlpLen(len(body), 0xc0), body...)
}
