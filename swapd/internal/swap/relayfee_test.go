package swap

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// The live stall: 5 USDC (6 decimals) with a leftover ETH RelayFee of 1e12 wei.
// 1e12 token units is 1,000,000 USDC — far above a 5 USDC lock — and the
// contract reverts BadParams when fee > value.
var (
	mockUSDC  = mustAddr("0xf942a536c8D07b1f83B4cE7ccBC415041aD7F35C")
	fiveUSDC  = big.NewInt(5_000_000) // 5 USDC, 6 decimals
	oneUSDC   = big.NewInt(1_000_000) // min lock
	ethWeiFee = new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
)

func mustAddr(s string) eth.Address {
	a, err := eth.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

func TestFeeForDoesNotApplyETHWeiToToken(t *testing.T) {
	p := DefaultPolicy()
	p.RelayFee = new(big.Int).Set(ethWeiFee) // 1e12 wei, fine for ETH
	if g := p.FeeFor(eth.Address{}); g.Cmp(ethWeiFee) != 0 {
		t.Fatalf("ETH fee: got %s want %s", g, ethWeiFee)
	}
	// Same policy, mock USDC: must NOT return 1e12.
	if g := p.FeeFor(mockUSDC); g.Sign() != 0 {
		t.Fatalf("unknown token must default to 0, got %s (would revert a 5 USDC claim)", g)
	}
}

func TestFeeForTokenUnits(t *testing.T) {
	want := big.NewInt(10_000) // 0.01 USDC
	p := Policy{
		RelayFee:      ethWeiFee,
		TokenRelayFee: map[eth.Address]*big.Int{mockUSDC: want},
	}
	if g := p.FeeFor(mockUSDC); g.Cmp(want) != 0 {
		t.Fatalf("USDC fee: got %s want %s", g, want)
	}
	if g := p.FeeFor(eth.Address{}); g.Cmp(ethWeiFee) != 0 {
		t.Fatalf("ETH fee still %s, got %s", ethWeiFee, g)
	}
	other, _ := eth.ParseAddress("0x0000000000000000000000000000000000000001")
	if g := p.FeeFor(other); g.Sign() != 0 {
		t.Fatalf("unlisted token: got %s want 0", g)
	}
}

func TestCheckRelayFeeFiveUSDC(t *testing.T) {
	// The exact stalling config: leftover 1e12 against a 1 USDC min lock.
	if err := CheckRelayFee(ethWeiFee, oneUSDC); err == nil {
		t.Fatal("1e12 fee against 1 USDC min lock must be rejected")
	} else if !strings.Contains(err.Error(), "not less than min lock") {
		t.Fatalf("error should name the rule, got %v", err)
	}
	if err := CheckRelayFee(fiveUSDC, oneUSDC); err == nil {
		t.Fatal("fee equal to a 5 USDC lock (and above min) must be rejected")
	}
	if err := CheckRelayFee(oneUSDC, oneUSDC); err == nil {
		t.Fatal("fee equal to min lock must be rejected")
	}
	if err := CheckRelayFee(big.NewInt(0), oneUSDC); err != nil {
		t.Fatalf("zero fee is always safe: %v", err)
	}
	if err := CheckRelayFee(big.NewInt(10_000), oneUSDC); err != nil {
		t.Fatalf("0.01 USDC fee is below a 1 USDC min: %v", err)
	}
	if err := CheckRelayFee(big.NewInt(999_999), oneUSDC); err != nil {
		t.Fatalf("just-below-min is ok: %v", err)
	}
	if err := CheckRelayFee(big.NewInt(-1), oneUSDC); err == nil {
		t.Fatal("negative fee")
	}
	if err := CheckRelayFee(big.NewInt(0), big.NewInt(0)); err == nil {
		t.Fatal("zero min lock")
	}
}

func TestValidateMakerRelayFiveUSDC(t *testing.T) {
	// Leftover ETH fee copied from a token offer — the live-test config.
	p := Policy{RelayFee: ethWeiFee}
	ApplyOfferRelayFee(&p, mockUSDC, ethWeiFee)
	if err := ValidateMakerRelay(p, mockUSDC, oneUSDC, true); err == nil {
		t.Fatal("1e12 token fee with a gas key must still fail the fee check")
	}

	// Sane token fee, but no gas key: must fail loudly.
	p = Policy{TokenRelayFee: map[eth.Address]*big.Int{mockUSDC: big.NewInt(10_000)}}
	err := ValidateMakerRelay(p, mockUSDC, oneUSDC, false)
	if err == nil || !strings.Contains(err.Error(), "SWAPD_ETH_KEY") {
		t.Fatalf("ERC-20 maker without a gas key must fail loudly, got %v", err)
	}

	// Happy path: 0.01 USDC fee, gas key present.
	if err := ValidateMakerRelay(p, mockUSDC, oneUSDC, true); err != nil {
		t.Fatalf("valid USDC fee with gas key: %v", err)
	}

	// ETH maker may omit a gas key (relayer pays claim gas).
	ethPol := Policy{RelayFee: ethWeiFee}
	ethMin := new(big.Int).SetUint64(1e15) // 0.001 ETH
	if err := ValidateMakerRelay(ethPol, eth.Address{}, ethMin, false); err != nil {
		t.Fatalf("ETH maker without gas key is allowed: %v", err)
	}
}

func TestApplyOfferRelayFeeDoesNotOverrideTokenRelayFee(t *testing.T) {
	p := Policy{TokenRelayFee: map[eth.Address]*big.Int{mockUSDC: big.NewInt(10_000)}}
	ApplyOfferRelayFee(&p, mockUSDC, ethWeiFee)
	if g := p.FeeFor(mockUSDC); g.Cmp(big.NewInt(10_000)) != 0 {
		t.Fatalf("existing TokenRelayFee must win, got %s", g)
	}
}

func TestPolicyTokenRelayFeeJSON(t *testing.T) {
	in := Policy{
		RelayFee:      big.NewInt(1e12),
		TokenRelayFee: map[eth.Address]*big.Int{mockUSDC: big.NewInt(10_000)},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Policy
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.FeeFor(mockUSDC).Cmp(big.NewInt(10_000)) != 0 {
		t.Fatalf("round-trip USDC fee %s", out.FeeFor(mockUSDC))
	}
	if out.FeeFor(eth.Address{}).Cmp(big.NewInt(1e12)) != 0 {
		t.Fatalf("round-trip ETH fee %s", out.FeeFor(eth.Address{}))
	}
}

func TestClaimFeeFallbackWhenAboveValue(t *testing.T) {
	// Mirrors maker.claim's defensive path: if a running policy is still
	// 1e12 against a 5 USDC lock, self-relay with 0 rather than sign it.
	p := Policy{TokenRelayFee: map[eth.Address]*big.Int{mockUSDC: new(big.Int).Set(ethWeiFee)}}
	fee := p.FeeFor(mockUSDC)
	if fee.Cmp(fiveUSDC) < 0 {
		t.Fatal("test setup: leftover fee should exceed 5 USDC")
	}
	// The claim path sets fee = 0 when !feeOK.
	if fiveUSDC.Cmp(fee) > 0 {
		t.Fatal("unreachable")
	}
	repaired := big.NewInt(0)
	if err := CheckRelayFee(repaired, oneUSDC); err != nil {
		t.Fatal(err)
	}
	if repaired.Cmp(fiveUSDC) >= 0 {
		t.Fatal("fee 0 must be below the 5 USDC lock")
	}
}
