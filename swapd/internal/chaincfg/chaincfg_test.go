package chaincfg

import "testing"

func TestDefaultIsArbitrumSepolia(t *testing.T) {
	ch := Resolve(Override{})
	if ch.Key != ArbitrumSepolia || ch.ID != 421614 {
		t.Fatalf("default chain: %+v", ch)
	}
	if ch.EthRPC == "" || ch.Explorer != "https://sepolia.arbiscan.io" {
		t.Fatalf("default RPCs/explorer: %+v", ch)
	}
	if ch.ETHUSDFeed == "" {
		t.Fatal("official ETH/USD on Arbitrum Sepolia should be in the table")
	}
	if ch.XMRUSDNote == "" {
		t.Fatal("should say the XMR/USD feed is a mock")
	}
	if ch.Contract != "0xf296e5b6f2268b700782FeCD3Da5b1Fd69CfceE3" || ch.USDC == "" || ch.XMRUSDFeed == "" {
		t.Fatalf("arb deploy addrs missing: %+v", ch)
	}
}

func TestLegacySepoliaRPCKept(t *testing.T) {
	ch := Resolve(Override{EthRPC: "https://ethereum-sepolia-rpc.publicnode.com", EthRPCs: []string{"https://1rpc.io/sepolia"}})
	if ch.Key != Sepolia || ch.ID != 11155111 {
		t.Fatalf("legacy RPC should stay on Sepolia: %+v", ch)
	}
	if ch.Contract != "0x178f93728ee58a3b8dc6ee8eefde41cb5519a389" {
		t.Fatalf("sepolia contract: %s", ch.Contract)
	}
	if ch.Explorer != "https://sepolia.etherscan.io" {
		t.Fatalf("explorer: %s", ch.Explorer)
	}
}

func TestLegacySepoliaContractKept(t *testing.T) {
	ch := Resolve(Override{Contract: "0x178F93728Ee58A3b8Dc6eE8EefDE41CB5519A389"})
	if ch.Key != Sepolia {
		t.Fatalf("got %s", ch.Key)
	}
}

func TestExplicitSepolia(t *testing.T) {
	ch := Resolve(Override{Chain: "sepolia"})
	if ch.Key != Sepolia || ch.ID != 11155111 {
		t.Fatalf("%+v", ch)
	}
}

func TestOverlayWins(t *testing.T) {
	ch := Resolve(Override{
		Chain:      ArbitrumSepolia,
		Contract:   "0x1111111111111111111111111111111111111111",
		USDC:       "0x2222222222222222222222222222222222222222",
		XMRUSDFeed: "0x3333333333333333333333333333333333333333",
		FairMaxBps: 250,
		EthRPC:     "https://example.invalid/arb",
	})
	if ch.Contract != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("contract overlay: %s", ch.Contract)
	}
	if ch.EthRPC != "https://example.invalid/arb" {
		t.Fatalf("rpc overlay: %s", ch.EthRPC)
	}
	if ch.FairMaxBps != 250 {
		t.Fatalf("bps: %d", ch.FairMaxBps)
	}
	if ch.ID != 421614 {
		t.Fatalf("id: %d", ch.ID)
	}
}

func TestCustomRPCNotForcedToDefault(t *testing.T) {
	ch := Resolve(Override{EthRPC: "https://example.invalid/hoodi"})
	if ch.Key != "custom" {
		t.Fatalf("custom RPC should not become %s", ch.Key)
	}
	if ch.EthRPC != "https://example.invalid/hoodi" {
		t.Fatalf("rpc replaced: %s", ch.EthRPC)
	}
}
