// Package feed reads AggregatorV3 (Chainlink-style) price feeds and compares
// a swap quote to a fair XMR/USD price.
package feed

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Check is the fair-price result shown on the quote screen.
type Check struct {
	Fair         float64 `json:"fairXmrUsd"`
	Implied      float64 `json:"impliedXmrUsd"`
	OffBps       int64   `json:"offBps"`
	MaxBps       int64   `json:"maxBps"`
	Warn         bool    `json:"warn"`
	Source       string  `json:"source"`
	Feed         string  `json:"feed,omitempty"`
	UpdatedAt    int64   `json:"updatedAt,omitempty"`
	ETHUSD       float64 `json:"ethUsd,omitempty"`
	ETHUSDSource string  `json:"ethUsdSource,omitempty"`
}

// Latest is one AggregatorV3 latestRoundData reading.
type Latest struct {
	Answer    *big.Int
	Decimals  uint8
	UpdatedAt time.Time
	Desc      string
}

// Price is Answer / 10^Decimals as a float64.
func (l Latest) Price() float64 {
	if l.Answer == nil || l.Decimals > 18 {
		return 0
	}
	f, _ := new(big.Float).SetInt(l.Answer).Float64()
	return f / math.Pow10(int(l.Decimals))
}

// Read calls latestRoundData, decimals, and description on an AggregatorV3.
func Read(ctx context.Context, c *eth.Client, addr eth.Address) (Latest, error) {
	var out Latest
	if c == nil || addr.IsZero() {
		return out, fmt.Errorf("no price feed")
	}
	raw, err := call(ctx, c, addr, "0xfeaf968c") // latestRoundData()
	if err != nil {
		return out, err
	}
	if len(raw) < 160 { // 5 * 32 bytes, hex without 0x is 320 chars = 160 bytes
		return out, fmt.Errorf("short latestRoundData")
	}
	// word 1 = answer (int256), word 3 = updatedAt
	out.Answer = wordInt(raw, 1)
	if out.Answer.Sign() <= 0 {
		return out, fmt.Errorf("feed answer is not positive")
	}
	out.UpdatedAt = time.Unix(wordInt(raw, 3).Int64(), 0)
	if dec, err := call(ctx, c, addr, "0x313ce567"); err == nil && len(dec) >= 32 {
		out.Decimals = uint8(wordInt(dec, 0).Uint64())
	} else {
		out.Decimals = 8
	}
	if desc, err := call(ctx, c, addr, "0x7284e416"); err == nil {
		out.Desc = decodeString(desc)
	}
	return out, nil
}

// ImpliedXMRUSD is the USD price of 1 XMR implied by a quote.
// USDC is treated as $1. ETH quotes need a live ETH/USD price.
func ImpliedXMRUSD(asset string, value *big.Int, xmrPico uint64, ethUSD float64) (float64, error) {
	if value == nil || value.Sign() <= 0 || xmrPico == 0 {
		return 0, fmt.Errorf("bad quote amounts")
	}
	pico := new(big.Float).SetUint64(xmrPico)
	switch strings.ToUpper(asset) {
	case "USDC":
		// (val / 1e6) / (pico / 1e12) = val * 1e6 / pico
		num := new(big.Float).SetInt(value)
		num.Mul(num, big.NewFloat(1e6))
		f, _ := num.Quo(num, pico).Float64()
		return f, nil
	default:
		if ethUSD <= 0 {
			return 0, fmt.Errorf("ETH/USD is required for an ETH quote")
		}
		// (ethUSD * wei / 1e18) / (pico / 1e12) = ethUSD * wei / (pico * 1e6)
		num := new(big.Float).SetInt(value)
		num.Mul(num, big.NewFloat(ethUSD))
		den := new(big.Float).SetUint64(xmrPico)
		den.Mul(den, big.NewFloat(1e6))
		f, _ := num.Quo(num, den).Float64()
		return f, nil
	}
}

// OffBps is the absolute difference between implied and fair, in basis points
// of the fair price. Returns 0 if fair is 0.
func OffBps(implied, fair float64) int64 {
	if fair == 0 {
		return 0
	}
	d := math.Abs(implied-fair) / fair * 10000
	return int64(math.Round(d))
}

// Evaluate builds a Check from already-fetched prices.
func Evaluate(fair, implied float64, maxBps int64, source, feedAddr string, updated time.Time) Check {
	if maxBps <= 0 {
		maxBps = 500
	}
	off := OffBps(implied, fair)
	return Check{
		Fair:      fair,
		Implied:   implied,
		OffBps:    off,
		MaxBps:    maxBps,
		Warn:      off > maxBps,
		Source:    source,
		Feed:      feedAddr,
		UpdatedAt: updated.Unix(),
	}
}

func call(ctx context.Context, c *eth.Client, addr eth.Address, data string) ([]byte, error) {
	var hexstr string
	err := c.Call(ctx, &hexstr, "eth_call", map[string]string{"to": addr.Hex(), "data": data}, "latest")
	if err != nil {
		return nil, err
	}
	hexstr = strings.TrimPrefix(hexstr, "0x")
	b, err := hex.DecodeString(hexstr)
	if err != nil {
		return nil, fmt.Errorf("bad eth_call: %w", err)
	}
	return b, nil
}

func wordInt(b []byte, i int) *big.Int {
	start := i * 32
	end := start + 32
	if end > len(b) {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(b[start:end])
}

func decodeString(b []byte) string {
	// ABI string: offset, length, bytes.
	if len(b) < 64 {
		return ""
	}
	off := int(new(big.Int).SetBytes(b[0:32]).Int64())
	if off+32 > len(b) {
		return ""
	}
	n := int(new(big.Int).SetBytes(b[off : off+32]).Int64())
	if n < 0 || off+32+n > len(b) {
		return ""
	}
	return string(b[off+32 : off+32+n])
}
