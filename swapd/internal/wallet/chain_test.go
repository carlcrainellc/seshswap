package wallet

import "testing"

func TestApplyChainDefaultsToArbSepolia(t *testing.T) {
	var c Config
	c.ApplyChain()
	if c.Chain != "arbitrum-sepolia" || c.ChainID != 421614 {
		t.Fatalf("%s %d", c.Chain, c.ChainID)
	}
	if c.EthRPC == "" || c.Explorer != "https://sepolia.arbiscan.io" {
		t.Fatalf("rpc/explorer %+v %s", c.EthRPC, c.Explorer)
	}
	if c.FairMaxBps != 500 {
		t.Fatalf("bps %d", c.FairMaxBps)
	}
	if c.ETHUSDFeed.IsZero() {
		t.Fatal("ETH/USD should be the official Arb Sepolia feed")
	}
	if c.XMRUSDNote == "" {
		t.Fatal("should disclose the mock XMR/USD feed")
	}
}

func TestApplyChainKeepsLegacySepolia(t *testing.T) {
	c := Config{EthRPC: "https://ethereum-sepolia-rpc.publicnode.com"}
	c.ApplyChain()
	if c.Chain != "sepolia" || c.ChainID != 11155111 {
		t.Fatalf("%s %d", c.Chain, c.ChainID)
	}
	if c.Explorer != "https://sepolia.etherscan.io" {
		t.Fatalf("explorer %s", c.Explorer)
	}
	if c.Contract.Hex() != "0x178f93728ee58a3b8dc6ee8eefde41cb5519a389" {
		t.Fatalf("contract %s", c.Contract.Hex())
	}
}
