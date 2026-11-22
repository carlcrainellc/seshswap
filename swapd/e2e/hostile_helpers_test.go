//go:build hostile

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

func randRead(b []byte) (int, error) { return rand.Read(b) }

// fund sends 1 ETH from the deployer to a (gas) key.
func fund(t *testing.T, e *Env, a eth.Address) {
	if _, err := e.eth.SendAndWait(e.ctx, deployerKey(), &a, big.NewInt(1e18), nil); err != nil {
		t.Fatal("fund:", err)
	}
}

// driveWarp is drive with a chain-time jump of `step` seconds per round (relay
// timeouts are measured in chain time).
func (e *Env) driveWarp(what string, maxRounds int, step uint64, cond func() bool, parties ...*Party) {
	for i := 0; i < maxRounds && !cond(); i++ {
		for _, p := range parties {
			p.eng.StepAll(e.ctx)
		}
		e.mineXMR(1)
		e.warp(step)
	}
	if !cond() {
		e.t.Fatalf("timeout: %s", what)
	}
}

func (e *Env) walletRPC(method string, params any) (json.RawMessage, error) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "0", "method": method, "params": params})
	resp, err := http.Post(e.walletMURL+"/json_rpc", "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

// rawTransferUnlock: maker wallet sends amt to addr with an unlock_time.
func (e *Env) rawTransferUnlock(addr string, amt, unlock uint64) error {
	if _, err := e.walletRPC("open_wallet", map[string]any{"filename": "main", "password": passphrase}); err != nil {
		return err
	}
	_, _ = e.walletRPC("refresh", nil)
	res, err := e.walletRPC("transfer", map[string]any{"destinations": []map[string]any{{"address": addr, "amount": amt}},
		"unlock_time": unlock, "get_tx_hex": false})
	if err != nil {
		return err
	}
	e.t.Logf("unlock_time transfer: %s", res)
	_, _ = e.walletRPC("close_wallet", nil)
	return nil
}
