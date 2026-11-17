package relay

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Live-test token and the 5 USDC lock that stalled with a 1e12 leftover fee.
var (
	mockUSDC  = mustAddr("0xf942a536c8D07b1f83B4cE7ccBC415041aD7F35C")
	fiveUSDC  = big.NewInt(5_000_000)
	tokenMin  = big.NewInt(10_000) // 0.01 USDC
	ethWeiFee = new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
)

func mustAddr(s string) eth.Address {
	a, err := eth.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

func usdcClaim(fee *big.Int) *eth.RelayedClaim {
	return &eth.RelayedClaim{
		Swap: &eth.Swap{Asset: mockUSDC, Value: new(big.Int).Set(fiveUSDC)},
		Fee:  fee,
	}
}

func TestCheckFeeFiveUSDC(t *testing.T) {
	s := &Server{TokenMinFee: map[eth.Address]*big.Int{mockUSDC: new(big.Int).Set(tokenMin)}}

	// The stalling claim: 1e12 token units on a 5 USDC lock.
	err := s.checkFee(usdcClaim(ethWeiFee), nil)
	if err == nil || !strings.Contains(err.Error(), "not less than locked value") {
		t.Fatalf("1e12 fee on 5 USDC must be rejected before simulation, got %v", err)
	}

	// Sane 0.01 USDC fee, at or above TokenMinFee and below the lock.
	if err := s.checkFee(usdcClaim(tokenMin), nil); err != nil {
		t.Fatalf("0.01 USDC fee should be accepted: %v", err)
	}

	// Below the relayer's minimum.
	err = s.checkFee(usdcClaim(big.NewInt(1)), nil)
	if err == nil || !strings.Contains(err.Error(), "fee too low") {
		t.Fatalf("below TokenMinFee: got %v", err)
	}

	// Zero fee is below TokenMinFee (relayer refuses; maker should self-relay).
	err = s.checkFee(usdcClaim(big.NewInt(0)), nil)
	if err == nil || !strings.Contains(err.Error(), "fee too low") {
		t.Fatalf("zero fee below min: got %v", err)
	}

	// Unknown token is refused (sane default: do not accept every ERC-20).
	other, _ := eth.ParseAddress("0x0000000000000000000000000000000000000001")
	err = s.checkFee(&eth.RelayedClaim{
		Swap: &eth.Swap{Asset: other, Value: fiveUSDC},
		Fee:  tokenMin,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("unknown token: got %v", err)
	}
}

func TestFeeQuoteEndpoint(t *testing.T) {
	s := &Server{TokenMinFee: map[eth.Address]*big.Int{mockUSDC: new(big.Int).Set(tokenMin)}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	fee, units, err := QuoteFee(context.Background(), srv.URL, mockUSDC)
	if err != nil {
		t.Fatal(err)
	}
	if units != "token" || fee.Cmp(tokenMin) != 0 {
		t.Fatalf("quote: fee %s units %s", fee, units)
	}

	// ETH is wei, estimated at submit time.
	fee, units, err = QuoteFee(context.Background(), srv.URL, eth.Address{})
	if err != nil {
		t.Fatal(err)
	}
	if units != "wei" || fee.Sign() != 0 {
		t.Fatalf("ETH quote: fee %s units %s", fee, units)
	}

	unknown, _ := eth.ParseAddress("0x0000000000000000000000000000000000000001")
	_, _, err = QuoteFee(context.Background(), srv.URL, unknown)
	if err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("unknown token quote: %v", err)
	}

	// Raw body is token-denominated.
	resp, err := http.Get(srv.URL + "/v1/fee?asset=" + mockUSDC.Hex())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var q FeeQuote
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || q.Units != "token" || q.Fee != "10000" {
		t.Fatalf("body %+v status %d", q, resp.StatusCode)
	}
}
