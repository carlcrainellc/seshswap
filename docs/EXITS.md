# Exits

Rent a Session Router exit by the hour from the wallet. Testnet only
(Arbitrum Sepolia).

- `ExitLease`: `0x145049b20f382Ca9D0B281a986dc02DEb653e65c`
- `SeshRegistry`: `0xc3576aefa11cef4e7758dacfa9e58560eca148d8` (tag `exit`)
- `MockUSDC`: `0x91280FA5450d481569C5fEBFdeF4E794C00cBCfa`
- `ExitPay`: `0xE622AF84dcC1f976f14E143287e68587CC8F24A6`

Connecting installs a default route through the tunnel. The exit carries web
traffic (TCP 80 and 443), its own DNS (`10.195.0.53`), and DNS to public
resolvers (UDP and TCP 53, rate-limited). Private address ranges and any other
ports are dropped while you're connected. Paid hours are final and don't renew.
They are not refunded if the exit goes down.

## Buying time

1. Install the exit build of the router: `scripts/fetch-session-router.sh`.
2. Open **Exits** in the wallet, pick an exit and the hours, press **Buy**
   (USDC or ETH), check the price on the confirm step and pay.
3. Press **Connect**. The wallet says Connected only once this computer's
   routes are up and a test request has gone through the exit (if none gets
   through in 30 seconds, it disconnects and says so); then it points DNS at the exit's resolver (`10.195.0.53`),
   and puts it back when you disconnect, lock or close the wallet, or if the
   routes drop. See *System DNS*.
4. Five minutes before your time ends, the wallet warns you and the
   **Add 1 hour** button turns bright. It shows the price and the new end
   time, and pays when you confirm: another hour on the same exit for the
   same router address, paid the way you paid last time (the other currency
   if the exit no longer takes it). The hour is added to
   the end of your current time and you stay connected. The button is there
   the whole time you're connected. Set `exitWarnBefore` in the wallet config
   (e.g. `"10m"`) to change when the warning shows.
5. The wallet disconnects when the time is up, or when you press
   **Disconnect**, lock the wallet or close it. Before it disconnects at the
   end, it checks the chain in case time was added another way.

The wallet's own chain RPCs keep working while connected only because they
are on 443: the wallet drops its open connections when the route changes and
the next request goes out through the exit. An RPC on another port (a local
node on 8545, say) can't be reached until you disconnect.

Connecting works on Linux only. The wallet uses the same router choice as
the private connection (Auto, Bundled, Existing, Direct). On a router you run
yourself it maps the exit but never stops or restarts the router.

## System DNS

With the default route on the tunnel, the wallet sends DNS lookups to the
exit's resolver too:

- With systemd-resolved, it sets the exit's resolver on the tunnel link with
  routing domain `~.` (`resolvectl dns/domain/default-route`). Disconnecting
  runs `resolvectl revert`; if the tunnel is gone, resolved has already
  dropped it.
- Otherwise it saves `/etc/resolv.conf` (or the symlink it was) and writes
  one with the exit's resolver. Disconnecting puts the original back, unless
  something else rewrote the file meanwhile.

The change is recorded before it is made. If the wallet is killed (even
`kill -9`), the next start puts DNS back before anything else.

Changing DNS needs root. The wallet runs its own binary as a small helper
through `sudo -n`, which needs this once (use your user and the wallet path):

```
echo 'alice ALL=(root) NOPASSWD: /opt/swap-wallet/swap-wallet __exit-dns *' | sudo tee /etc/sudoers.d/swap-wallet-dns
```

The helper only changes DNS for the user who ran sudo and keeps its state in
`/var/lib/swap-wallet-dns`. Without the rule the wallet connects anyway and
shows the resolver to set by hand. `"exitDns": "off"` in the wallet config
skips the DNS change only; the default route still moves to the tunnel.

## Paying with XMR

A maker can have its swap claims buy exit time. Add to the maker config:

```json
"exitPay": {
  "factory": "0xE622AF84dcC1f976f14E143287e68587CC8F24A6",
  "listingId": 4,
  "hours": 24,
  "maxCost": 600000000000000,
  "buyerSesh": "<your router>.sesh"
}
```

`maxCost` is in wei. Each claim goes to its own `ExitPay` address, which buys
the hours and sends the change to `payoutTo` (or `refundTo`, if set). If the
lease can't be bought, everything goes there instead. To retry a settlement:
`swapd exit-pay-settle -config F SWAP_ID`.

## Selling time

Run this as root on an Ubuntu 24.04 server. `cli.json` is your swapd client config
and `N` is the listing id that `register` prints.

```bash
cp swapd/bin/swapd /opt/srnet-exit/bin/swapd
seller-kit/exit-seller.sh install      # prints the exit .sesh

META=$(swapd exits meta -price "0.10 USDC/h" -payout 0xPAYOUT -country DE)
swapd registry register -config cli.json -sesh "$(cat /opt/srnet-exit/config/exit.sesh)" \
  -tag exit -bond 0.0002 -meta "$META"
swapd exits offer -config cli.json -id N -payout 0xPAYOUT -usdc-per-hour 0.10 -eth-per-hour 0.00002 -max-hours 24

seller-kit/exit-seller.sh install --listing N   # starts the watcher
```

The kit runs the exit router, an nftables NAT for 80/443, an unbound resolver
on the tunnel, forwarded DNS to public resolvers (100/s per client, burst 200;
private ranges stay blocked), and a watcher that admits buyers from
`ExitLease` events until their time ends. The wallet still uses the exit's
resolver; Rusty on Windows sends DNS to 1.1.1.1, which this forwarding is for.
On hosts without systemd, use `exit-seller.sh up` and `exit-seller.sh down`.

To let a client in without payment, and skip rate limits for them:

```bash
seller-kit/exit-seller.sh allow <their-address>.sesh
seller-kit/exit-seller.sh allowed
seller-kit/exit-seller.sh disallow <their-address>.sesh
```

The list is kept in `/opt/srnet-exit/config/allowed`. You can also pass
`--unlimited <addr>.sesh` (repeatable) at `install` or `render`. The kit
restarts the exit if it is already running; you do not need to reinstall.
There is no data cap. Do not put keys in the file, only `.sesh` addresses.

Forwarding and NAT are set up by the kit: `net.ipv4.ip_forward`, an nftables
masquerade for the exit range out of the WAN interface, and the same for IPv6
when the host has an IPv6 default route (`--ipv6 auto|on|off`). Running it
again changes nothing. The exit service runs `exit-seller.sh check` before it
starts and refuses to start if forwarding or NAT is missing, with the reason
and the fix. `exit-seller.sh uninstall` removes the units, the NAT and the
sysctl file and puts forwarding back the way it was; keys stay in the prefix.
`seller-kit/test/nat-test.sh` tests all of this in throwaway namespaces.

A watchdog (`swapd exits watchdog`) checks one thing for the seller: does
this exit forward? It says nothing about whether a buyer's own routes are up;
the buyer's wallet checks that itself. It checks the exit the way a buyer uses it.
The kit runs a second, small router (the probe) in its own network namespace
(`exitprobe`, a /30 to this host, NATed out like the buyers). The exit lets
the probe's key in through its whitelist. That key is made on the seller's
host when the kit is installed (`config/probe.key`) and never leaves it; it
is not in the packages or the kit. Every 30 seconds the probe looks up
`example.com` with the exit's resolver and does a TLS handshake with it on
443, both through the exit (`-probe-host` picks another site). A plain
request from the host itself would not do: it never enters the tunnel.

After 10 failed probes in a row (5 minutes) the watchdog restarts the exit
router with the same key, so the exit keeps its `.sesh` and buyers reconnect
by themselves. Before that it maps the exit again (after 2 failures) and
restarts the probe router once (after 5), so a broken probe is not blamed on
the exit. Restarts are rate-limited: none before the probe has worked once,
none within 5 minutes of a start, at least 15 minutes between restarts, and
that gap doubles (up to 2 hours) while restarts don't help. Every probe
failure and decision is logged, with an hourly count of probes answered and
failed (`journalctl -u srnet-exit-watchdog`, or `log/watchdog.log` with
`up`). Turn it off with `--watchdog off`; tune it with
`--watchdog-args "-fails 6 -min-gap 30m"`.

What it does not catch: problems that only some buyers see (their own
relays, their side of the network), and sites other than the probe site
being blocked. The probe and the exit share the host's uplink, so when the
host loses the network the watchdog restarts the exit even though that
can't help; the backoff keeps that to a few restarts. The router's status
line is no help: an exit that only answers buyers always shows `0↑`
outbound paths, healthy or not.

The watcher reads the chain through a list of RPCs, primary first
(`--rpc URL,URL2`; by default swapd's built-in Arbitrum Sepolia primary and
fallback). An RPC that times out (10 s per try) or fails is skipped for the
next one at once; the watcher stays on the one that works and tries the
primary again after 5 minutes.

The kit installs the router build in `dist/session-router-srnetx6/`. To use
another build, pass `--build NAME` (looks in `dist/session-router-NAME/`) or
`--debs DIR`. `scripts/fetch-session-router.sh` reads the same name from
`SR_BUILD`.
