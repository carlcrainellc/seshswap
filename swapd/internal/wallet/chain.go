package wallet

import (
	"github.com/carlcrainellc/seshswap/swapd/internal/chaincfg"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// ApplyChain fills empty RPC, contract, USDC, feed, and explorer fields from
// the built-in chain table. The default chain is Arbitrum Sepolia. A config
// that already has Sepolia RPCs or the Sepolia contract stays on Sepolia.
func (c *Config) ApplyChain() {
	ch := chaincfg.Resolve(chaincfg.Override{
		Chain:      c.Chain,
		EthRPC:     c.EthRPC,
		EthRPCs:    c.EthRPCs,
		Contract:   addrStr(c.Contract),
		USDC:       addrStr(c.USDC),
		XMRUSDFeed: addrStr(c.XMRUSDFeed),
		ETHUSDFeed: addrStr(c.ETHUSDFeed),
		Explorer:   c.Explorer,
		FairMaxBps: c.FairMaxBps,
	})
	c.Chain = ch.Key
	if c.ChainID == 0 {
		c.ChainID = ch.ID
	}
	c.ChainLabel = ch.Label
	if c.Explorer == "" {
		c.Explorer = ch.Explorer
	}
	if c.EthRPC == "" {
		c.EthRPC = ch.EthRPC
		if len(c.EthRPCs) == 0 {
			c.EthRPCs = append([]string{}, ch.EthRPCs...)
		}
	}
	if c.Contract.IsZero() {
		c.Contract = mustAddr(ch.Contract)
	}
	if c.USDC.IsZero() {
		c.USDC = mustAddr(ch.USDC)
	}
	if c.XMRUSDFeed.IsZero() {
		c.XMRUSDFeed = mustAddr(ch.XMRUSDFeed)
	}
	if c.ETHUSDFeed.IsZero() {
		c.ETHUSDFeed = mustAddr(ch.ETHUSDFeed)
	}
	if c.XMRUSDNote == "" {
		c.XMRUSDNote = ch.XMRUSDNote
	}
	if c.FairMaxBps == 0 {
		c.FairMaxBps = ch.FairMaxBps
	}
	// The registry is bound to one swap contract: only default it when the
	// wallet uses that contract.
	if c.Registry.IsZero() && !c.NoRegistry && ch.Registry != "" && c.Contract == mustAddr(ch.Contract) {
		c.Registry = mustAddr(ch.Registry)
		if c.RegistryFromBlock == 0 {
			c.RegistryFromBlock = ch.RegistryFromBlock
		}
	}
	if c.ExitLease.IsZero() && !c.Registry.IsZero() && ch.ExitLease != "" && c.Registry == mustAddr(ch.Registry) {
		c.ExitLease = mustAddr(ch.ExitLease)
		if c.ExitLeaseFromBlock == 0 {
			c.ExitLeaseFromBlock = ch.ExitLeaseFromBlock
		}
	}
	if c.USDCDecimals == 0 && ch.USDCDecimals != 0 {
		c.USDCDecimals = ch.USDCDecimals
	}
}

func addrStr(a eth.Address) string {
	if a.IsZero() {
		return ""
	}
	return a.Hex()
}

func mustAddr(s string) eth.Address {
	if s == "" {
		return eth.Address{}
	}
	a, err := eth.ParseAddress(s)
	if err != nil {
		return eth.Address{}
	}
	return a
}
