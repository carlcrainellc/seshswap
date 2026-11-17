package wallet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
)

// A connection near its end warns once, offers a top-up at the quoted
// price, and the top-up pays for the same .sesh and keeps the connection.
func TestExitExpiryWarningAndTopUp(t *testing.T) {
	s, f := exitTestServer(t)
	h := s.Handler()
	ctx := context.Background()
	s.Cfg.Transport = ModeAuto
	s.Cfg.ExitWarnBefore = "10m"
	buyer, err := s.buyerSesh(ctx, exitRoute{Source: SourceBundled})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, h, "POST", "/v1/exits/topup", `{}`, ""); code != 409 {
		t.Fatalf("top-up without a connection: %d", code)
	}
	end := uint64(time.Now().Add(2 * time.Second).Unix())
	addLease(s, buyer, end)
	chainEnd := end
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return chainEnd, nil }
	s.ex.priceFor = func(_ context.Context, id uint64, asset string, hours uint32) (string, error) {
		if asset == "usdc" {
			return "0.10 USDC", nil
		}
		return "0.00002 ETH", nil
	}
	var bought []string
	s.ex.buyFor = func(_ context.Context, id uint64, asset string, hours uint32, who string) (*exits.Lease, string, error) {
		bought = append(bought, asset)
		if who != buyer || id != 4 || hours != 1 {
			t.Errorf("bought for %s on %d, %d h", who, id, hours)
		}
		if asset == "eth" {
			return nil, "", errors.New("this exit doesn't take that currency")
		}
		chainEnd = end + 3600 // ExitLease adds the hour to the end of the current time
		return &exits.Lease{LeaseID: 9, ListingID: id, BuyerSesh: who, Hours: hours, Start: end, End: chainEnd, Amount: "100000"}, "https://x/tx/9", nil
	}
	if _, err := s.connectExit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	s.exitTick()
	st := s.exitsStatusMap()
	c := st["connected"].(map[string]any)
	if c["expiring"] != true || !strings.Contains(st["last"].(string), "Add an hour") {
		t.Fatalf("no warning: %v / %v", c, st["last"])
	}
	s.refreshTopUpPrice(ctx)
	if c := s.exitsStatusMap()["connected"].(map[string]any); c["topUpPrice"] != "0.00002 ETH" {
		t.Fatalf("price %v", c["topUpPrice"])
	}
	code, r := do(t, h, "POST", "/v1/exits/topup", `{}`, "")
	if code != 200 {
		t.Fatalf("top-up %d %v", code, r)
	}
	if strings.Join(bought, ",") != "eth,usdc" {
		t.Fatalf("paid like last time, then the other currency: %v", bought)
	}
	c = r["connected"].(map[string]any)
	if uint64(c["end"].(float64)) != end+3600 || c["expiring"] != false || !strings.Contains(r["last"].(string), "Added 1 hour") {
		t.Fatalf("after top-up %v %v", c, r["last"])
	}
	if c["asset"] != "usdc" {
		t.Fatalf("asset %v", c["asset"])
	}
	// The old end passes: still connected, nothing unmapped.
	time.Sleep(time.Until(time.Unix(int64(end), 0)) + 50*time.Millisecond)
	s.exitTick()
	if !s.exitHoldsRouter() || f.seen("llarp.unmap_exit") != 0 {
		t.Fatalf("disconnected at the old end: %v", f.calls)
	}
	if n := len(s.exitsStatusMap()["leases"].([]map[string]any)); n != 2 {
		t.Fatalf("leases %d", n)
	}
}

// Time added elsewhere (another device, a swap claim) is picked up at the
// old end instead of disconnecting.
func TestExitEndRechecksChain(t *testing.T) {
	s, f := exitTestServer(t)
	ctx := context.Background()
	s.Cfg.Transport = ModeAuto
	buyer, _ := s.buyerSesh(ctx, exitRoute{Source: SourceBundled})
	end := uint64(time.Now().Add(time.Second).Unix())
	addLease(s, buyer, end)
	chainEnd := end
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return chainEnd, nil }
	if _, err := s.connectExit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	chainEnd = end + 7200
	time.Sleep(time.Until(time.Unix(int64(end), 0)) + 50*time.Millisecond)
	s.exitTick()
	if !s.exitHoldsRouter() || f.seen("llarp.unmap_exit") != 0 || !strings.Contains(s.exitsStatusMap()["last"].(string), "extended") {
		t.Fatalf("should have stayed: %v %v", f.calls, s.exitsStatusMap()["last"])
	}
}
