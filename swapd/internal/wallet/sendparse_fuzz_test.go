package wallet

import (
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

// FuzzSendInputs: amounts and Monero addresses typed into Send.
func FuzzSendInputs(f *testing.F) {
	f.Add("1,000.25", uint8(18), "5", uint8(2))
	f.Add("0.0000001", uint8(6), "", uint8(0))
	f.Add(" .5 ", uint8(12), "4", uint8(1))
	f.Fuzz(func(t *testing.T, v string, d uint8, addr string, netSel uint8) {
		dec := []int{6, 12, 18}[int(d)%3]
		if x, err := parseSendAmount(v, dec); err == nil {
			if x.Sign() <= 0 {
				t.Fatalf("parseSendAmount(%q) = %v", v, x)
			}
			// never rounds: the amount shown back is the amount typed
			back, err := parseSendAmount(formatUnits(x, dec), dec)
			if err != nil || back.Cmp(x) != 0 {
				t.Fatalf("%q -> %v -> %q -> %v", v, x, formatUnits(x, dec), back)
			}
			clean := strings.TrimSpace(strings.ReplaceAll(v, ",", ""))
			if i := strings.IndexByte(clean, '.'); i >= 0 && len(clean)-i-1 > dec {
				t.Fatalf("%q has more than %d decimals but was accepted", v, dec)
			}
		}
		want := []keys.Network{keys.Mainnet, keys.Stagenet, keys.Testnet, keys.Regtest}[int(netSel)%4]
		if err := checkXmrAddress(addr, want); err == nil {
			net, _, perr := keys.ParseAddress(addr)
			if perr != nil || (net != want && !(want == keys.Regtest && net == keys.Mainnet)) {
				t.Fatalf("checkXmrAddress accepted %q for %s (%s, %v)", addr, want, net, perr)
			}
		}
	})
}
