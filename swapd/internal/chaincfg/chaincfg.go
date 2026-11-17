// Package chaincfg holds per-chain RPC, contract, and price-feed settings
// for the wallet, swapd, and the relayer.
//
// The default chain is Arbitrum Sepolia (testnet). Sepolia is kept and is
// selected automatically for legacy configs that already point at it.
// Mainnet chains are refused elsewhere (wallet and swapd mainnets maps).
package chaincfg

import (
	"strings"
)

const (
	Sepolia           = "sepolia"
	ArbitrumSepolia   = "arbitrum-sepolia"
	DefaultKey        = ArbitrumSepolia
	DefaultFairMaxBps = 500 // 5%
)

// Chain is one EVM testnet the wallet and swapd can talk to.
type Chain struct {
	Key          string   `json:"key"`
	ID           int64    `json:"id"`
	Label        string   `json:"label"`
	Explorer     string   `json:"explorer"`
	EthRPC       string   `json:"ethRpc"`
	EthRPCs      []string `json:"ethRpcs,omitempty"`
	Contract     string   `json:"contract,omitempty"`
	USDC         string   `json:"usdc,omitempty"`
	XMRUSDFeed   string   `json:"xmrUsdFeed,omitempty"`
	ETHUSDFeed   string   `json:"ethUsdFeed,omitempty"`
	USDCDecimals int      `json:"usdcDecimals,omitempty"`
	FairMaxBps   int      `json:"fairMaxBps,omitempty"`
	// XMRUSDNote explains the XMR/USD source. On Arbitrum Sepolia there is
	// no official Chainlink XMR/USD feed; a mock aggregator is used instead.
	XMRUSDNote string `json:"xmrUsdNote,omitempty"`
	// SeshRegistry for Contract (bonded makers and .sesh listings).
	Registry          string `json:"registry,omitempty"`
	RegistryFromBlock uint64 `json:"registryFromBlock,omitempty"`
	// ExitLease: buy hours on exits listed in Registry with tag "exit".
	ExitLease          string `json:"exitLease,omitempty"`
	ExitLeaseFromBlock uint64 `json:"exitLeaseFromBlock,omitempty"`
}

// Override is the explicit config a JSON file already set. Non-empty fields
// win over the built-in table.
type Override struct {
	Chain      string
	EthRPC     string
	EthRPCs    []string
	Contract   string
	USDC       string
	XMRUSDFeed string
	ETHUSDFeed string
	Explorer   string
	FairMaxBps int
}

func builtins() map[string]Chain {
	return map[string]Chain{
		Sepolia: {
			Key:      Sepolia,
			ID:       11155111,
			Label:    "Sepolia",
			Explorer: "https://sepolia.etherscan.io",
			EthRPC:   "https://ethereum-sepolia-rpc.publicnode.com",
			EthRPCs:  []string{"https://1rpc.io/sepolia"},
			Contract: "0x178f93728ee58a3b8dc6ee8eefde41cb5519a389",
			USDC:     "0xf942a536c8D07b1f83B4cE7ccBC415041aD7F35C",
			// No official Chainlink XMR/USD on Ethereum Sepolia either.
			ETHUSDFeed:   "0x694AA1769357215DE4FAC081bf1f309aDC325306",
			USDCDecimals: 6,
			FairMaxBps:   DefaultFairMaxBps,
		},
		ArbitrumSepolia: {
			Key:      ArbitrumSepolia,
			ID:       421614,
			Label:    "Arbitrum Sepolia",
			Explorer: "https://sepolia.arbiscan.io",
			EthRPC:   "https://sepolia-rollup.arbitrum.io/rpc",
			EthRPCs:  []string{"https://arbitrum-sepolia-rpc.publicnode.com"},
			// Deployed 13 Nov 2026 (CT). Official Chainlink ETH/USD exists
			// on this chain. There is no official Chainlink XMR/USD (the
			// Arbitrum Sepolia feed list 404s); XMRUSDFeed is a mock AggregatorV3.
			Contract:     "0x1F3e5b5d9C7bE3a95535Bac1e68864B1d10A46F4",
			USDC:         "0x91280FA5450d481569C5fEBFdeF4E794C00cBCfa",
			XMRUSDFeed:   "0xaeF4c8ae583e245323e3C72dea975B0d81FEBc8b",
			ETHUSDFeed:   "0xd30e2101a97dcbAeBCBC04F14C3f624E67A35165",
			USDCDecimals: 6,
			FairMaxBps:   DefaultFairMaxBps,
			XMRUSDNote:   "a mock XMR/USD feed (no official Chainlink XMR/USD on Arbitrum Sepolia)",
			// SeshRegistry, deployed 15 Nov 2026 (CT) with the placeholder
			// testnet parameters in docs/REGISTRY.md.
			Registry:          "0xc3576aefa11cef4e7758dacfa9e58560eca148d8",
			RegistryFromBlock: 329381312,
			// ExitLease, deployed 15 Nov 2026 (CT), verified on Sourcify.
			ExitLease:          "0x145049b20f382Ca9D0B281a986dc02DEb653e65c",
			ExitLeaseFromBlock: 329627329,
		},
	}
}

// Lookup returns a built-in chain by key.
func Lookup(key string) (Chain, bool) {
	ch, ok := builtins()[strings.ToLower(strings.TrimSpace(key))]
	return ch, ok
}

// Resolve picks a chain and overlays explicit JSON fields. An empty chain
// key defaults to Arbitrum Sepolia, unless the RPCs or contract already
// belong to Sepolia (legacy configs keep working) or are a custom endpoint.
func Resolve(o Override) Chain {
	key := strings.ToLower(strings.TrimSpace(o.Chain))
	if key == "" {
		key = infer(o)
	}
	var base Chain
	if key == "" {
		base = Chain{Key: "custom", Label: "Ethereum testnet", FairMaxBps: DefaultFairMaxBps}
	} else if ch, ok := Lookup(key); ok {
		base = ch
	} else {
		base = Chain{Key: key, Label: key, FairMaxBps: DefaultFairMaxBps}
	}
	return overlay(base, o)
}

func infer(o Override) string {
	for _, ch := range builtins() {
		if o.Contract != "" && equalAddr(o.Contract, ch.Contract) {
			return ch.Key
		}
	}
	blob := strings.ToLower(o.EthRPC + " " + strings.Join(o.EthRPCs, " "))
	if strings.Contains(blob, "sepolia-rollup.arbitrum") ||
		strings.Contains(blob, "arbitrum-sepolia") ||
		(strings.Contains(blob, "arbitrum") && strings.Contains(blob, "sepolia")) {
		return ArbitrumSepolia
	}
	if strings.Contains(blob, "sepolia") {
		return Sepolia
	}
	if o.EthRPC != "" || o.Contract != "" || len(o.EthRPCs) > 0 {
		return "" // custom; do not force the default
	}
	return DefaultKey
}

func overlay(base Chain, o Override) Chain {
	if o.EthRPC != "" {
		base.EthRPC = o.EthRPC
		if len(o.EthRPCs) > 0 {
			base.EthRPCs = append([]string{}, o.EthRPCs...)
		}
	} else if len(o.EthRPCs) > 0 {
		base.EthRPC = o.EthRPCs[0]
		base.EthRPCs = append([]string{}, o.EthRPCs[1:]...)
	}
	if o.Contract != "" {
		base.Contract = o.Contract
	}
	if o.USDC != "" {
		base.USDC = o.USDC
	}
	if o.XMRUSDFeed != "" {
		base.XMRUSDFeed = o.XMRUSDFeed
	}
	if o.ETHUSDFeed != "" {
		base.ETHUSDFeed = o.ETHUSDFeed
	}
	if o.Explorer != "" {
		base.Explorer = strings.TrimRight(o.Explorer, "/")
	}
	if o.FairMaxBps > 0 {
		base.FairMaxBps = o.FairMaxBps
	}
	if base.FairMaxBps == 0 {
		base.FairMaxBps = DefaultFairMaxBps
	}
	return base
}

func equalAddr(a, b string) bool {
	a = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(a), "0x"))
	b = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(b), "0x"))
	return a != "" && a == b
}
