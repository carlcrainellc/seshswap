package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

// RegistryConfig is the optional "registry" block (maker and CLI).
type RegistryConfig struct {
	Contract     eth.Address `json:"contract"`
	FromBlock    uint64      `json:"fromBlock,omitempty"`    // deployment block (for list/search)
	ListingID    uint64      `json:"listingId,omitempty"`    // maker: our listing
	AttestWindow uint64      `json:"attestWindow,omitempty"` // maker: seconds for our ack (0 = contract minimum)
}

const registryUsage = `swapd registry: manage a listing in SeshRegistry (testnets only)

  swapd registry register  -config F -sesh NAME.sesh -tag maker|site|forum|... -meta TEXT -bond ETH
  swapd registry topup     -config F -id N -amount ETH
  swapd registry update    -config F -id N -meta TEXT
  swapd registry exit      -config F -id N          start the withdrawal delay (listing hidden)
  swapd registry cancel-exit -config F -id N
  swapd registry withdraw  -config F -id N [-to ADDR]  after the delay
  swapd registry answer    -config F -id N          answer a challenge on your listing
  swapd registry challenge -config F -id N          challenge someone's listing (posts the challenge bond)
  swapd registry resolve   -config F -id N          settle an unanswered challenge
  swapd registry info      -config F -id N
  swapd registry list      -config F [-q TEXT] [-tag TAG] [-all]
  swapd registry deploy    -config F -artifact contracts/out/SeshRegistry.sol/SeshRegistry.json

The listing key comes from SWAPD_REGISTRY_KEY (hex), never from a file
argument. For a maker it is the key in the maker's registry config: it signs
the swap commitments and acks.`

func parseETH(s string) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("bad ETH amount %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	if !r.IsInt() {
		return nil, fmt.Errorf("too many decimals in %q", s)
	}
	return r.Num(), nil
}

func runRegistry(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, registryUsage)
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("registry "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", "", "config JSON (ethRpc/chain and a registry block)")
	sesh := fs.String("sesh", "", "listing .sesh name")
	tag := fs.String("tag", "", "category tag")
	meta := fs.String("meta", "", "short description or metadata URI (max 280 bytes)")
	bond := fs.String("bond", "", "bond in ETH")
	amount := fs.String("amount", "", "top-up in ETH")
	id := fs.Uint64("id", 0, "listing id")
	to := fs.String("to", "", "withdraw destination (default: the listing key's address)")
	q := fs.String("q", "", "list: search words")
	all := fs.Bool("all", false, "list: include listings that left or were removed")
	art := fs.String("artifact", "", "deploy: forge artifact JSON")
	_ = fs.Parse(args[1:])
	cfg := mustConfig(*cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := eth.NewClients(append([]string{cfg.EthRPC}, cfg.EthRPCs...)...)
	cid, err := c.ChainID(ctx)
	check(err)
	if name, ok := mainnets[cid.Int64()]; ok {
		log.Fatalf("refusing %s mainnet: SeshRegistry is test-only", name)
	}
	var key *eth.Key
	if h := os.Getenv("SWAPD_REGISTRY_KEY"); h != "" {
		key, err = eth.KeyFromHex(h)
		check(err)
	}
	needKey := func() *eth.Key {
		if key == nil {
			log.Fatal("set SWAPD_REGISTRY_KEY (hex) for this command")
		}
		return key
	}
	var regAddr eth.Address
	if cfg.Registry != nil {
		regAddr = cfg.Registry.Contract
	}
	if regAddr.IsZero() && sub != "deploy" {
		log.Fatal("config needs registry.contract")
	}
	rc := &registry.Client{C: c, Addr: regAddr}
	send := func(what string, value *big.Int, data []byte) *eth.Receipt {
		k := needKey()
		r, err := c.SendAndWait(ctx, k, &regAddr, value, data)
		if err != nil {
			log.Fatalf("%s: %v", what, friendlyRevert(err))
		}
		fmt.Printf("%s: tx %s/tx/%s (gas %d)\n", what, cfg.Explorer, r.TxHash.Hex(), r.Gas())
		return r
	}
	needID := func() uint64 {
		if *id == 0 {
			log.Fatal("-id is required")
		}
		return *id
	}

	switch sub {
	case "deploy":
		k := needKey()
		if *art == "" {
			log.Fatal("-artifact is required")
		}
		b, err := os.ReadFile(*art)
		check(err)
		var j struct {
			Bytecode struct {
				Object string `json:"object"`
			} `json:"bytecode"`
		}
		check(json.Unmarshal(b, &j))
		code, err := hex.DecodeString(strings.TrimPrefix(j.Bytecode.Object, "0x"))
		check(err)
		p := registry.PlaceholderParams(cfg.Contract, cfg.USDC, cfg.ETHUSDFeed)
		if cfg.USDC.IsZero() {
			p.USDCDecimals, p.ETHUSDFeed, p.MaxFeedAge = 0, eth.Address{}, 0
		}
		r, err := c.SendAndWait(ctx, k, nil, nil, append(code, p.ConstructorArgs()...))
		check(err)
		fmt.Printf("SeshRegistry deployed at %s in block %d (tx %s/tx/%s)\n", r.Contract.Hex(), r.Block(), cfg.Explorer, r.TxHash.Hex())
		fmt.Printf("placeholder params: %+v\n", p)
	case "register":
		if !registry.ValidSesh(*sesh) {
			log.Fatal("-sesh must be <label>.sesh, lowercase letters, digits and '-'")
		}
		t, err := registry.Tag(*tag)
		check(err)
		v, err := parseETH(*bond)
		check(err)
		before, _ := rc.Count(ctx)
		send("register", v, registry.EncodeRegister(*sesh, t, *meta))
		fmt.Printf("listing id %d (if no one else registered in the same block)\n", before+1)
	case "topup":
		v, err := parseETH(*amount)
		check(err)
		send("top up", v, registry.EncodeTopUp(needID()))
	case "update":
		send("update", nil, registry.EncodeUpdateMeta(needID(), *meta))
	case "exit":
		send("exit", nil, registry.EncodeRequestExit(needID()))
	case "cancel-exit":
		send("cancel exit", nil, registry.EncodeCancelExit(needID()))
	case "withdraw":
		dest := needKey().Address()
		if *to != "" {
			dest, err = eth.ParseAddress(*to)
			check(err)
		}
		send("withdraw", nil, registry.EncodeWithdraw(needID(), dest))
	case "answer":
		send("answer", nil, registry.EncodeAnswer(needID()))
	case "challenge":
		b, err := rc.C.EthCall(ctx, eth.CallMsg{To: &regAddr, Data: eth.Selector("challengeBond()")})
		check(err)
		send("challenge", new(big.Int).SetBytes(b), registry.EncodeChallenge(needID()))
	case "resolve":
		send("resolve", nil, registry.EncodeResolve(needID()))
	case "info":
		in, err := rc.Info(ctx, needID())
		check(err)
		fmt.Printf("listing %d: owner %s, %s, tag %q, bond %s ETH (held for open swaps %s), max swap %s ETH",
			*id, in.Owner.Hex(), registry.StatusName(in.Status), in.Tag, registry.FormatWei(in.Bond),
			registry.FormatWei(in.Reserved), registry.FormatWei(in.MaxQuote))
		if in.ExitAt != 0 {
			fmt.Printf(", can withdraw from %s", time.Unix(int64(in.ExitAt), 0).Format("Jan 2 3:04 PM MST"))
		}
		if !in.Challenger.IsZero() {
			fmt.Printf(", CHALLENGED by %s", in.Challenger.Hex())
		}
		fmt.Println()
	case "list":
		from := uint64(0)
		if cfg.Registry != nil {
			from = cfg.Registry.FromBlock
		}
		ix := registry.OpenIndex("", regAddr, from)
		check(ix.Sync(ctx, c))
		for _, e := range ix.Search(*q, *tag, *all) {
			fmt.Printf("#%d  %-8s %-10s bond %-8s %s  %s\n", e.ID, e.Tag, registry.StatusName(e.Status), registry.FormatWei(e.BondWei()), e.Sesh, e.Meta)
		}
	default:
		fmt.Fprintln(os.Stderr, registryUsage)
		os.Exit(2)
	}
}

func friendlyRevert(err error) error {
	s := err.Error()
	for sel, msg := range map[string]string{
		"LowBond": "bond too small for this tag (makers need the larger bond)", "Taken": "that .sesh name is already listed",
		"BadName": "bad .sesh name", "NotOwner": "only the listing owner can do that", "WrongTime": "too early (delay not over, or swaps still open)",
		"WrongStatus": "the listing isn't in a state that allows this", "Challenged_": "the listing has an open challenge",
	} {
		if strings.Contains(s, hex.EncodeToString(eth.Selector(sel+"()"))) {
			return fmt.Errorf("%s (%s)", msg, sel)
		}
	}
	return err
}
