package registry

import (
	"math/big"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// DeployParams mirrors SeshRegistry.Params. Every amount here is a
// PLACEHOLDER for testnets; the real numbers are Max's call.
type DeployParams struct {
	Swap            eth.Address
	SiteMinBond     *big.Int
	MakerMinBond    *big.Int
	CapFactor       uint64
	PenaltyBps      uint64
	MinPenalty      *big.Int
	ChallengeBond   *big.Int
	ExitDelay       uint64
	ChallengeWindow uint64
	MinAttest       uint64
	MaxAttest       uint64
	USDC            eth.Address
	USDCDecimals    uint64
	ETHUSDFeed      eth.Address
	MaxFeedAge      uint64
}

func ether(f string) *big.Int {
	r, _ := new(big.Rat).SetString(f)
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	return new(big.Int).Quo(r.Num(), r.Denom())
}

// PlaceholderParams are the testnet values used for the Arbitrum Sepolia
// deployment (and the forge tests). PLACEHOLDERS, not a recommendation.
func PlaceholderParams(swap, usdc, ethUsd eth.Address) DeployParams {
	return DeployParams{
		Swap:            swap,
		SiteMinBond:     ether("0.0002"),  // any non-maker listing
		MakerMinBond:    ether("0.002"),   // makers
		CapFactor:       5,                // biggest swap a maker may quote = bond x 5
		PenaltyBps:      500,              // penalty >= 5% of the swap's ETH value...
		MinPenalty:      ether("0.00005"), // ...and at least this
		ChallengeBond:   ether("0.0001"),
		ExitDelay:       3 * 24 * 3600, // 3 days
		ChallengeWindow: 2 * 24 * 3600, // 2 days to answer a challenge
		MinAttest:       10 * 60,       // maker's ack window: 10 to 30 minutes
		MaxAttest:       30 * 60,
		USDC:            usdc,
		USDCDecimals:    6,
		ETHUSDFeed:      ethUsd,
		MaxFeedAge:      2 * 24 * 3600,
	}
}

// ConstructorArgs is abi.encode(Params) (a static tuple).
func (p DeployParams) ConstructorArgs() []byte {
	var b []byte
	b = append(b, eth.WordAddr(p.Swap)...)
	b = append(b, eth.WordBig(p.SiteMinBond)...)
	b = append(b, eth.WordBig(p.MakerMinBond)...)
	b = append(b, eth.WordU(p.CapFactor)...)
	b = append(b, eth.WordU(p.PenaltyBps)...)
	b = append(b, eth.WordBig(p.MinPenalty)...)
	b = append(b, eth.WordBig(p.ChallengeBond)...)
	b = append(b, eth.WordU(p.ExitDelay)...)
	b = append(b, eth.WordU(p.ChallengeWindow)...)
	b = append(b, eth.WordU(p.MinAttest)...)
	b = append(b, eth.WordU(p.MaxAttest)...)
	b = append(b, eth.WordAddr(p.USDC)...)
	b = append(b, eth.WordU(p.USDCDecimals)...)
	b = append(b, eth.WordAddr(p.ETHUSDFeed)...)
	b = append(b, eth.WordU(p.MaxFeedAge)...)
	return b
}
