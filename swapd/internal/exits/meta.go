package exits

import (
	"fmt"
	"sort"
	"strings"
)

// Meta is what an exit listing says about itself in SeshRegistry's 280-byte
// meta field: "exit; price=0.10 USDC/h; chain=arbitrum-sepolia; payout=0x…;
// country=US; policy=tcp/80,tcp/443,dns; dns=10.195.0.53; note=…".
//
// It is for display and search only. The price that is charged is the
// on-chain ExitLease offer, which the wallet always reads before paying.
type Meta struct {
	Price   string            `json:"price,omitempty"`   // e.g. "0.10 USDC/h" (display)
	Chain   string            `json:"chain,omitempty"`   // e.g. "arbitrum-sepolia"
	Payout  string            `json:"payout,omitempty"`  // 0x… (must match the on-chain offer)
	Country string            `json:"country,omitempty"` // ISO 3166 alpha-2
	Policy  string            `json:"policy,omitempty"`  // e.g. "tcp/80,tcp/443,dns"
	DNS     string            `json:"dns,omitempty"`     // the exit's own resolver, reached through the tunnel
	Note    string            `json:"note,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
}

// MaxMeta is SeshRegistry.MAX_META.
const MaxMeta = 280

// String renders the meta field. It fails if it would not fit.
func (m Meta) String() (string, error) {
	parts := []string{"exit"}
	add := func(k, v string) {
		v = strings.TrimSpace(strings.NewReplacer(";", ",", "\n", " ").Replace(v))
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("price", m.Price)
	add("chain", m.Chain)
	add("payout", m.Payout)
	add("country", strings.ToUpper(m.Country))
	add("policy", m.Policy)
	add("dns", m.DNS)
	keys := make([]string, 0, len(m.Extra))
	for k := range m.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k, m.Extra[k])
	}
	add("note", m.Note)
	s := strings.Join(parts, "; ")
	if len(s) > MaxMeta {
		return "", fmt.Errorf("listing text is %d bytes; the registry allows %d", len(s), MaxMeta)
	}
	return s, nil
}

// ParseMeta reads a meta field. Unknown keys go to Extra; text without "=" is ignored.
func ParseMeta(s string) Meta {
	var m Meta
	for _, p := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "price":
			m.Price = v
		case "chain":
			m.Chain = v
		case "payout":
			m.Payout = v
		case "country":
			m.Country = strings.ToUpper(v)
		case "policy":
			m.Policy = v
		case "dns":
			m.DNS = v
		case "note":
			m.Note = v
		default:
			if m.Extra == nil {
				m.Extra = map[string]string{}
			}
			m.Extra[k] = v
		}
	}
	return m
}
