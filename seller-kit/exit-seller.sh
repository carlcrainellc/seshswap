#!/usr/bin/env bash
# Exit seller kit: sell hours on a lease-gated Session Router exit, paid through ExitLease
# and listed in SeshRegistry (tag "exit"). Testnets only.
#
# Exit mode, NAT, web (TCP 80/443), the exit's own resolver on the tunnel, DNS to public
# resolvers (rate-limited), and a watcher that admits paying buyers. Buyers pay on-chain
# (ExitLease); the listing lives in SeshRegistry. A host can also admit a client .sesh
# without payment (allow / disallow).
#
#   exit-seller.sh render  [options]   write configs under PREFIX (no root needed)
#   exit-seller.sh install [options]   Ubuntu 24.04 + systemd: packages, configs, units (root)
#   exit-seller.sh up      [options]   hosts without systemd: start everything in the background (root)
#   exit-seller.sh down    [options]   stop what "up" started, remove the NAT, restore forwarding (root)
#   exit-seller.sh uninstall [options] systemd: stop and remove the units, NAT and sysctl file (root);
#                                      keys and data under PREFIX stay
#   exit-seller.sh nat-up  [options]   forwarding + NAT only (idempotent; run by install and up)
#   exit-seller.sh nat-down [options]  remove the NAT, put forwarding back how it was
#   exit-seller.sh check   [options]   is forwarding and NAT in place? exit 1 with the reason if not
#   exit-seller.sh restart-exit [opts] restart only the exit router, same key (used by the watchdog)
#   exit-seller.sh restart-probe [opts] restart the watchdog's probe router (used by the watchdog)
#   exit-seller.sh status  [options]
#   exit-seller.sh allow   <addr>.sesh   admit this client with no payment (repeatable)
#   exit-seller.sh disallow <addr>.sesh  remove a host-granted client
#   exit-seller.sh allowed               list host-granted clients
#
# Settings given at install/up/render are saved in PREFIX/config/kit.env, so later commands
# (check, nat-up, down, uninstall, allow) use them without repeating the flags.
# Host-granted addresses are kept in PREFIX/config/allowed.
#
# Options (env or flags):
#   --prefix DIR        install root (default /opt/srnet-exit)
#   --listing N         SeshRegistry listing id of this exit (set after "swapd exits ... register")
#   --wan IF            internet-facing interface (default: the default route's)
#   --port N            router UDP port (default 1090)
#   --tun NAME          exit TUN (default sr-tun-seller)
#   --dns-ip IP         exit resolver address, tunnel-only (default 10.195.0.53)
#   --dns-forward IP    test hosts only: forward the resolver to IP instead of recursing from the roots
#   --allow-net CIDR    test hosts only: let buyers reach CIDR even though it is a private range
#                       (needed where the host's own egress is a proxy on a reserved range)
#   --build NAME        patched Session Router build to install (default srnetx6)
#   --debs DIR          its debs (default: ../dist/session-router-BUILD)
#   --swapd PATH        swapd binary for the watcher (default: PREFIX/bin/swapd)
#   --rpc URL[,URL...]  Arbitrum Sepolia RPCs, primary first; the watcher moves to the next one when
#                       an RPC times out or fails (default: swapd's built-in primary + fallback)
#   --unbound PATH      unbound command (default /usr/sbin/unbound)
#   --ipv6 auto|on|off  forward buyers' IPv6 too (auto: on when this host has an IPv6 default route)
#   --watchdog on|off   probe the exit end to end and restart its router when it stops answering
#                       (default on). The probe is a second, small router in its own network
#                       namespace (--probe-ns, default exitprobe) that the exit lets in by key.
#   --watchdog-args A   extra flags for "swapd exits watchdog" (e.g. "-fails 10 -min-gap 30m")
#   --probe-ns NAME     network namespace for the watchdog's probe router (default exitprobe)
#   --unlimited ADDR    same as "allow ADDR" when rendering or installing (repeatable)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
CMD=${1:-}; shift || true
ARGS=()
UNLIMITED_ADD=()
while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX=$2; shift 2;;
    --listing) LISTING=$2; shift 2;;
    --wan) WAN_IF=$2; shift 2;;
    --port) PORT=$2; shift 2;;
    --tun) TUN=$2; shift 2;;
    --dns-ip) DNS_IP=$2; shift 2;;
    --dns-forward) DNS_FORWARD=$2; shift 2;;
    --allow-net) ALLOW_NET=$2; shift 2;;
    --build) SR_BUILD=$2; shift 2;;
    --debs) DEBS=$2; shift 2;;
    --swapd) SWAPD=$2; shift 2;;
    --rpc) RPC=$2; shift 2;;
    --unbound) UNBOUND=$2; shift 2;;
    --ipv6) IPV6=$2; shift 2;;
    --watchdog) WATCHDOG=$2; shift 2;;
    --watchdog-args) WATCHDOG_ARGS=$2; shift 2;;
    --probe-ns) PROBE_NS=$2; shift 2;;
    --unlimited)
      [ -n "${2:-}" ] || { echo "--unlimited needs a .sesh address" >&2; exit 2; }
      UNLIMITED_ADD+=("$2"); shift 2;;
    --*) echo "unknown option $1" >&2; exit 2;;
    *) ARGS+=("$1"); shift;;
  esac
done
PREFIX=${PREFIX:-/opt/srnet-exit}
# Saved settings fill in whatever the flags and environment left empty.
[ -f "$PREFIX/config/kit.env" ] && . "$PREFIX/config/kit.env"
: "${LISTING:=0}" "${WAN_IF:=}" "${PORT:=1090}" "${TUN:=sr-tun-seller}" "${DNS_IP:=10.195.0.53}"
: "${DNS_FORWARD:=}" "${ALLOW_NET:=}" "${SR_BUILD:=srnetx6}" "${DEBS:=}" "${SWAPD:=}" "${RPC:=}"
: "${UNBOUND:=/usr/sbin/unbound}" "${IPV6:=auto}" "${WATCHDOG:=on}" "${WATCHDOG_ARGS:=}" "${PROBE_NS:=exitprobe}"
TUN_NET4=10.193.0.0/16
TUN_NET6=fd2e:7365:7369::/64
# the probe router's namespace: a /30 to this host, NATed out like the buyers
PROBE_NET4=10.199.255.0/30; PROBE_HOST_IP=10.199.255.1; PROBE_NS_IP=10.199.255.2
PROBE_VETH=exprb0; PROBE_VETH_NS=exprb1
SWAPD=${SWAPD:-$PREFIX/bin/swapd}
DEBS=${DEBS:-$HERE/../dist/session-router-$SR_BUILD}
[ -n "$WAN_IF" ] || WAN_IF=$(ip -4 route show default | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
C=$PREFIX/config; RUN=$PREFIX/run; DATA=$PREFIX/data; LOG=$PREFIX/log; ST=$PREFIX/state
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}
SYSCTL_FILE=${SYSCTL_FILE:-/etc/sysctl.d/60-exit-seller.conf}
case "$IPV6" in auto|on|off) ;; *) echo "--ipv6 must be auto, on or off" >&2; exit 2;; esac

die() { echo "exit-seller: $*" >&2; exit 1; }
need_root() { [ "$(id -u)" = 0 ] || die "run as root (sudo)"; }

# Host-granted clients: admitted with no payment, pinned to a fixed tunnel IP so
# nftables can skip rate limits for them. There is no data cap in the kit; a
# whitelist hit also has no lease clock. Stored in $C/allowed.
ALLOWED_FILE() { echo "$C/allowed"; }

valid_sesh() {
  python3 -c 'import sys
s=sys.argv[1]
ok=len(s)==57 and s.endswith(".sesh") and all((("a"<=c<="z" and c not in "lv") or c=="1" or "3"<=c<="9") for c in s[:52])
sys.exit(0 if ok else 1)' "$1"
}

# Merge --unlimited addresses into $C/allowed, assigning fixed tunnel IPs as needed.
# Safe to run before or after the file exists.
sync_allowed() {
  mkdir -p "$C"
  local f; f=$(ALLOWED_FILE)
  python3 - "$f" "${UNLIMITED_ADD[@]+"${UNLIMITED_ADD[@]}"}" <<'PY'
import os, sys
path = sys.argv[1]
adds = sys.argv[2:]
exit_v4, exit_v6 = "10.193.0.1", "fd2e:7365:7369::1"
rows = []  # (sesh, v4, v6)
seen = set()
if os.path.exists(path):
    for line in open(path):
        s = line.strip()
        if not s or s.startswith("#"):
            continue
        p = s.split()
        if len(p) < 3:
            continue
        addr, v4, v6 = p[0], p[1], p[2]
        if addr in seen:
            continue
        seen.add(addr)
        rows.append([addr, v4, v6])
used4 = {r[1] for r in rows} | {exit_v4, "10.193.0.0", "10.193.255.255"}
used6 = {r[2] for r in rows} | {exit_v6}

def valid(s):
    return len(s)==57 and s.endswith(".sesh") and all((("a"<=c<="z" and c not in "lv") or c=="1" or "3"<=c<="9") for c in s[:52])

def dec4(ip):
    a,b,c,d = map(int, ip.split("."))
    n = (a<<24)|(b<<16)|(c<<8)|d
    n -= 1
    return f"{(n>>24)&255}.{(n>>16)&255}.{(n>>8)&255}.{n&255}"

def dec6(ip):
    if ip.startswith("fd2e:7365:7369:"):
        hexpart = ip.split("::")[-1] if "::" in ip else ip.split(":")[-1]
        if hexpart in ("", "fd2e:7365:7369"):
            n = 0
        else:
            n = int(hexpart, 16)
        n -= 1
        if n < 2:
            raise SystemExit("no free pinned IPv6 left in fd2e:7365:7369::2-ffff")
        return f"fd2e:7365:7369::{n:x}"
    raise SystemExit(f"unexpected IPv6 pin {ip}")

def next4():
    ip = "10.193.255.254"
    while ip in used4:
        if ip == "10.193.0.2":
            raise SystemExit("no free pinned IPv4 left in 10.193.0.2-10.193.255.254")
        ip = dec4(ip)
    used4.add(ip)
    return ip

def next6():
    ip = "fd2e:7365:7369::ffff"
    while ip in used6:
        ip = dec6(ip)
    used6.add(ip)
    return ip

for a in adds:
    if not valid(a):
        raise SystemExit(f"not a client .sesh address: {a}")
    if a in seen:
        continue
    v4, v6 = next4(), next6()
    rows.append([a, v4, v6])
    seen.add(a)

rows.sort(key=lambda r: r[0])
text = (
    "# Client .sesh addresses this host admits without payment.\n"
    "# Each line: ADDRESS.sesh  IPV4  IPV6\n"
    "# Managed by exit-seller.sh allow / disallow / --unlimited. Do not share keys, only .sesh addresses.\n"
)
for addr, v4, v6 in rows:
    text += f"{addr}  {v4}  {v6}\n"
os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
tmp = path + ".tmp"
with open(tmp, "w") as f:
    f.write(text)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
PY
}

# Print "addr v4 v6" lines (no comments).
allowed_rows() {
  local f; f=$(ALLOWED_FILE)
  [ -f "$f" ] || return 0
  awk 'NF>=3 && $1 !~ /^#/ {print $1, $2, $3}' "$f"
}

allow_one() {
  local addr=$1
  valid_sesh "$addr" || die "not a client .sesh address: $addr"
  if allowed_rows | awk -v a="$addr" '$1==a {found=1} END{exit !found}'; then
    echo "already allowed: $addr"
  else
    UNLIMITED_ADD+=("$addr")
    sync_allowed
    echo "allowed $addr (no payment; rate limits skipped)"
    allowed_rows | awk -v a="$addr" '$1==a {print "  tunnel IP: "$2, $3}'
  fi
}

disallow_one() {
  local addr=$1 f
  valid_sesh "$addr" || die "not a client .sesh address: $addr"
  f=$(ALLOWED_FILE)
  [ -f "$f" ] || die "$addr is not on the allow list"
  if ! allowed_rows | awk -v a="$addr" '$1==a {found=1} END{exit !found}'; then
    die "$addr is not on the allow list"
  fi
  python3 - "$f" "$addr" <<'PY'
import os, sys
path, addr = sys.argv[1], sys.argv[2]
rows = []
for line in open(path):
    s = line.strip()
    if not s or s.startswith("#"):
        continue
    p = s.split()
    if len(p) >= 3 and p[0] != addr:
        rows.append((p[0], p[1], p[2]))
text = (
    "# Client .sesh addresses this host admits without payment.\n"
    "# Each line: ADDRESS.sesh  IPV4  IPV6\n"
    "# Managed by exit-seller.sh allow / disallow / --unlimited. Do not share keys, only .sesh addresses.\n"
)
for a, v4, v6 in rows:
    text += f"{a}  {v4}  {v6}\n"
tmp = path + ".tmp"
with open(tmp, "w") as f:
    f.write(text)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
PY
  echo "removed $addr from the allow list"
}

list_allowed() {
  if ! allowed_rows | grep -q .; then
    echo "no host-granted clients"
    return 0
  fi
  echo "host-granted clients (no payment, rate limits skipped):"
  allowed_rows | while read -r a v4 v6; do
    echo "  $a  $v4  $v6"
  done
}

# After allow/disallow: rewrite configs and, if we can, reload NAT and restart the exit.
apply_allowed() {
  render >/dev/null
  if [ "$(id -u)" != 0 ]; then
    echo "config written under $PREFIX; as root run: $0 nat-up --prefix $PREFIX"
    echo "and restart the exit (systemctl restart srnet-exit, or exit-seller.sh restart-exit) so the list takes effect."
    return 0
  fi
  if nft list table inet srnet_seller >/dev/null 2>&1; then
    nft -f "$C/nat.nft"
  fi
  if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet srnet-exit 2>/dev/null; then
    systemctl restart srnet-exit
    echo "exit router restarted so the new allow list takes effect"
  elif [ -f "$RUN/exit.pid" ] && kill -0 "$(cat "$RUN/exit.pid")" 2>/dev/null; then
    PUB=$(issuer_pub)
    stop_bg exit
    start_exit
    echo "exit router restarted so the new allow list takes effect"
  else
    echo "exit is not running; the list will apply when it starts"
  fi
}


# IPv6 is forwarded when asked, or (auto) when this host can reach the IPv6 internet.
use_v6() {
  case "$IPV6" in
    on) return 0;; off) return 1;;
    *) [ -n "$(ip -6 route show default 2>/dev/null)" ];;
  esac
}

render() {
  mkdir -p "$C" "$RUN" "$DATA" "$LOG" "$ST" "$PREFIX/bin"
  chmod 700 "$C" "$RUN" "$ST"
  # flags > environment > this file > defaults
  {
    echo "# exit-seller.sh settings (written by render; edit or re-run with flags)"
    for v in LISTING WAN_IF PORT TUN DNS_IP DNS_FORWARD ALLOW_NET SR_BUILD SWAPD RPC UNBOUND IPV6 WATCHDOG WATCHDOG_ARGS PROBE_NS; do
      printf ': "${%s:=%s}"\n' "$v" "${!v}"
    done
  } > "$C/kit.env.tmp" && chmod 600 "$C/kit.env.tmp" && mv "$C/kit.env.tmp" "$C/kit.env"
  sync_allowed
  local probe_wl="" host_wl="" un4_el="" un6_el=""
  [ -s "$C/probe.sesh" ] && probe_wl="# the watchdog's probe router (this host)
auth-whitelist=$(cat "$C/probe.sesh")"
  while read -r a v4 v6; do
    [ -n "$a" ] || continue
    host_wl="${host_wl}# host-granted (no payment)
auth-whitelist=$a
mapaddr=$a:$v4,$v6
"
    un4_el="${un4_el}${un4_el:+, }$v4"
    un6_el="${un6_el}${un6_el:+, }$v6"
  done < <(allowed_rows)
  cat > "$C/exit.ini" <<INI
# Exit seller kit: lease-gated exit. Signed admissions
# (SR_EXIT_LEASE_ADMISSIONS / SR_EXIT_LEASE_ISSUER_PUBKEY), the watchdog's
# probe router, and host-granted clients (auth-whitelist / allow) get a session.
[router]
data-dir=$DATA

[logging]
level=info

[exit]
enable=true
routed-range=public
# Web (TCP 80/443), this exit's resolver ($DNS_IP), and DNS to public
# resolvers (nftables rate-limits forwarded DNS; private ranges stay blocked).
policy=tcp/443
policy=tcp/80
policy=udp/53
policy=tcp/53

[network]
keyfile=$C/exit.key
ifname=$TUN
ifaddr=10.193.0.1/16
ifaddr=fd2e:7365:7369::1/64
reachable=true
auth-type=whitelist
$probe_wl
$host_wl
auto-routing=false
blackhole-routes=false

[api]
enabled=true
bind=ipc://$RUN/exit-rpc.sock

[bind]
listen=:$PORT

[dns]
listen=127.0.0.1:29153
INI

  cat > "$C/probe.ini" <<INI
# Watchdog probe: a client router in namespace $PROBE_NS that maps this exit for
# one address ($DNS_IP, the exit's resolver) and nothing else.
[router]
data-dir=$DATA/probe

[logging]
level=warn

[network]
keyfile=$C/probe.key
reachable=false
ifname=sr-tun-probe
ifaddr=10.194.0.1/16
auto-routing=true
blackhole-routes=false

[api]
enabled=true
bind=ipc://$RUN/probe-rpc.sock

[dns]
listen=127.0.0.1:29154
no-resolvconf=1

[bind]
listen=0.0.0.0:0
INI

  local allow="" v6set="" v6fwd="    iifname \"$TUN\" meta nfproto ipv6 drop" v6nat="" v6dns="" v6unl=""
  [ -n "$ALLOW_NET" ] && allow="    iifname \"$TUN\" ip daddr $ALLOW_NET tcp dport { 80, 443 } oifname \"$WAN_IF\" accept"
  local un4set="  set unlimited4 { type ipv4_addr; }"
  [ -n "$un4_el" ] && un4set="  set unlimited4 { type ipv4_addr; elements = { $un4_el }; }"
  local un6set="  set unlimited6 { type ipv6_addr; }"
  [ -n "$un6_el" ] && un6set="  set unlimited6 { type ipv6_addr; elements = { $un6_el }; }"
  if use_v6; then
    v6set="  set blocked6 {
    type ipv6_addr; flags interval
    elements = { ::/127, ::ffff:0:0/96, 64:ff9b::/96, 100::/64, 2001:db8::/32, fc00::/7, fe80::/10, ff00::/8 }
  }
  set dnsfwd_rl6 { type ipv6_addr; size 65535; flags dynamic,timeout; timeout 1m; }"
    v6fwd="    iifname \"$TUN\" ip6 daddr @blocked6 drop"
    v6nat="    ip6 saddr $TUN_NET6 oifname \"$WAN_IF\" counter masquerade"
    v6unl="    iifname \"$TUN\" oifname \"$WAN_IF\" meta l4proto { tcp, udp } th dport 53 ip6 saddr @unlimited6 counter accept"
    v6dns="    iifname \"$TUN\" oifname \"$WAN_IF\" meta l4proto { tcp, udp } th dport 53 update @dnsfwd_rl6 { ip6 saddr limit rate over 100/second burst 200 packets } drop"
  fi
  cat > "$C/nat.nft" <<NFT
table inet srnet_seller
delete table inet srnet_seller
table inet srnet_seller {
  set blocked4 {
    type ipv4_addr; flags interval
    elements = { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,
                 192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/3 }
  }
$v6set
$un4set
$un6set
  set dns_rl4 { type ipv4_addr; size 65535; flags dynamic,timeout; timeout 1m; }
  set dnsfwd_rl4 { type ipv4_addr; size 65535; flags dynamic,timeout; timeout 1m; }
  chain forward_filter {
    type filter hook forward priority -10; policy accept;
$allow
    iifname "$TUN" ip daddr @blocked4 drop
$v6fwd
    iifname "$TUN" oifname "$WAN_IF" meta l4proto { tcp, udp } th dport 53 ip saddr @unlimited4 counter accept
$v6unl
    iifname "$TUN" oifname "$WAN_IF" meta l4proto { tcp, udp } th dport 53 update @dnsfwd_rl4 { ip saddr limit rate over 100/second burst 200 packets } drop
$v6dns
    iifname "$TUN" oifname "$WAN_IF" meta l4proto { tcp, udp } th dport 53 counter accept
    iifname "$TUN" oifname "$WAN_IF" tcp dport { 80, 443 } counter accept
    iifname "$TUN" drop
    oifname "$TUN" ct state established,related accept
    oifname "$TUN" drop
  }
  chain input_filter {
    type filter hook input priority -10; policy accept;
    ip daddr $DNS_IP iifname != { "$TUN", "lo" } drop
    iifname "$TUN" ct state established,related accept
    iifname "$TUN" ip daddr $DNS_IP meta l4proto { tcp, udp } th dport 53 ip saddr @unlimited4 counter accept
    iifname "$TUN" ip daddr $DNS_IP meta l4proto { tcp, udp } th dport 53 update @dns_rl4 { ip saddr limit rate over 100/second burst 200 packets } drop
    iifname "$TUN" ip daddr $DNS_IP meta l4proto { tcp, udp } th dport 53 counter accept
    iifname "$TUN" drop
  }
  chain postrouting_nat {
    type nat hook postrouting priority 100;
    ip saddr $TUN_NET4 oifname "$WAN_IF" counter masquerade
    ip saddr $PROBE_NET4 oifname "$WAN_IF" masquerade
$v6nat
  }
}
NFT

  local fwd="" mod='"validator iterator"' anchor="  auto-trust-anchor-file: \"$DATA/root.key\""
  if [ -n "$DNS_FORWARD" ]; then
    # Test hosts whose network only allows DNS through the host resolver.
    fwd="forward-zone:
  name: \".\"
  forward-addr: $DNS_FORWARD"
    mod='"iterator"'; anchor=""
  fi
  cat > "$C/unbound.conf" <<CONF
# Exit resolver for leased buyers: answers only the tunnel, no query logs.
server:
  interface: $DNS_IP
  port: 53
  ip-freebind: yes
  do-ip4: yes
  do-ip6: no
  access-control: 0.0.0.0/0 refuse
  access-control: ::0/0 refuse
  access-control: $TUN_NET4 allow
  access-control: $DNS_IP/32 allow
  username: ""
  chroot: ""
  directory: "$DATA"
  pidfile: ""
  use-syslog: no
  logfile: ""
  verbosity: 0
  log-queries: no
  log-replies: no
  module-config: $mod
$anchor
  qname-minimisation: yes
  hide-identity: yes
  hide-version: yes
  minimal-responses: yes
  deny-any: yes
  num-threads: 1
  ip-ratelimit: 100
  private-address: 10.0.0.0/8
  private-address: 172.16.0.0/12
  private-address: 192.168.0.0/16
  private-address: 169.254.0.0/16
  private-address: 100.64.0.0/10
  private-address: fd00::/8
  private-address: fe80::/10
remote-control:
  control-enable: no
$fwd
CONF

  RPC="$RPC" python3 - "$C/swapd.json" <<'PY'
import json, os, sys
cfg = {"chain": "arbitrum-sepolia"}
rpcs = [u.strip() for u in os.environ.get("RPC", "").split(",") if u.strip()]
if rpcs:
    cfg["ethRpc"] = rpcs[0]
    if rpcs[1:]:
        cfg["ethRpcs"] = rpcs[1:]
json.dump(cfg, open(sys.argv[1], "w"))
PY
  echo "rendered configs in $C"
}

router_key() { "$SWAPD" exits router-key -out "$C/exit.key" > "$C/exit.sesh"; }
probe_key() { "$SWAPD" exits router-key -out "$C/probe.key" > "$C/probe.sesh"; }

# The probe router's namespace: veth to this host, default route, NAT (in nat.nft).
probe_ns_up() {
  ip netns list | grep -qw "$PROBE_NS" || ip netns add "$PROBE_NS"
  if ! ip link show "$PROBE_VETH" >/dev/null 2>&1; then
    ip link add "$PROBE_VETH" type veth peer name "$PROBE_VETH_NS"
    ip link set "$PROBE_VETH_NS" netns "$PROBE_NS"
  fi
  ip addr replace "$PROBE_HOST_IP/30" dev "$PROBE_VETH"; ip link set "$PROBE_VETH" up
  ip -n "$PROBE_NS" addr replace "$PROBE_NS_IP/30" dev "$PROBE_VETH_NS"
  ip -n "$PROBE_NS" link set "$PROBE_VETH_NS" up; ip -n "$PROBE_NS" link set lo up
  ip -n "$PROBE_NS" route replace default via "$PROBE_HOST_IP"
  mkdir -p "/etc/netns/$PROBE_NS"
  grep -v '^nameserver 127\.' /etc/resolv.conf > "/etc/netns/$PROBE_NS/resolv.conf" || true
  grep -q '^nameserver' "/etc/netns/$PROBE_NS/resolv.conf" || echo "nameserver 9.9.9.9" >> "/etc/netns/$PROBE_NS/resolv.conf"
  mkdir -p "$DATA/probe"
}
probe_ns_down() {
  ip link del "$PROBE_VETH" 2>/dev/null || true
  ip netns del "$PROBE_NS" 2>/dev/null || true
  rm -rf "/etc/netns/$PROBE_NS"
}

issuer_pub() { "$SWAPD" exits keygen -issuer-key "$C/issuer.key"; }

exit_sesh() {
  # The router's own .sesh: the localhost.sesh CNAME on its DNS listener.
  python3 - <<'PY'
import socket, os, struct
q = os.urandom(2) + b"\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00" + b"\x09localhost\x04sesh\x00\x00\x05\x00\x01"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
s.sendto(q, ("127.0.0.1", 29153)); d, _ = s.recvfrom(1500)
i = 12
while d[i]: i += d[i] + 1
i += 5
if struct.unpack(">H", d[6:8])[0] == 0: raise SystemExit("no answer yet")
if d[i] & 0xC0 == 0xC0: i += 2
else:
    while d[i]: i += d[i] + 1
    i += 1
i += 10
labels = []
while d[i]:
    if d[i] & 0xC0 == 0xC0: break
    labels.append(d[i+1:i+1+d[i]].decode()); i += d[i] + 1
name = ".".join(labels)
print(name if name.endswith(".sesh") else name + ".sesh")
PY
}

# Docker sets the FORWARD policy to DROP; let the exit's TUN through DOCKER-USER.
docker_allow() {
  local t ipt
  for t in iptables ip6tables; do
    [ $t = ip6tables ] && ! use_v6 && continue
    ipt=$(command -v $t || true); [ -n "$ipt" ] || continue
    "$ipt" -S DOCKER-USER >/dev/null 2>&1 || continue
    for d in -i -o; do
      "$ipt" -C DOCKER-USER $d "$TUN" -j ACCEPT 2>/dev/null || "$ipt" -I DOCKER-USER $d "$TUN" -j ACCEPT
      [ $t = iptables ] || continue
      "$ipt" -C DOCKER-USER $d "$PROBE_VETH" -j ACCEPT 2>/dev/null || "$ipt" -I DOCKER-USER $d "$PROBE_VETH" -j ACCEPT
    done
  done
}
docker_unallow() {
  local t ipt
  for t in iptables ip6tables; do
    ipt=$(command -v $t || true); [ -n "$ipt" ] || continue
    for d in -i -o; do
      while "$ipt" -D DOCKER-USER $d "$TUN" -j ACCEPT 2>/dev/null; do :; done
      while "$ipt" -D DOCKER-USER $d "$PROBE_VETH" -j ACCEPT 2>/dev/null; do :; done
    done
  done
}

# ---------------------------------------------------------------- forwarding + NAT
P4=/proc/sys/net/ipv4/ip_forward
P6=/proc/sys/net/ipv6/conf/all/forwarding
rd() { cat "$1" 2>/dev/null || echo "?"; }

# nat-up: turn on forwarding and load the NAT table. Safe to run any number of times:
# the table is replaced as a whole, and the values forwarding had before the kit first
# touched it are kept in STATE/forwarding.before for nat-down.
nat_up() {
  [ -f "$C/nat.nft" ] || die "no $C/nat.nft; run render first"
  mkdir -p "$ST"
  if [ ! -f "$ST/forwarding.before" ]; then
    printf 'v4=%s\nv6=%s\n' "$(rd $P4)" "$(rd $P6)" > "$ST/forwarding.before"
  fi
  echo 1 > $P4
  if use_v6; then
    # With forwarding on, the kernel ignores router advertisements unless accept_ra=2;
    # keep the WAN's own IPv6 default route.
    local ra=/proc/sys/net/ipv6/conf/$WAN_IF/accept_ra
    if [ "$(rd "$ra")" = 1 ]; then
      grep -q "^ra_$WAN_IF=" "$ST/forwarding.before" || echo "ra_$WAN_IF=1" >> "$ST/forwarding.before"
      echo 2 > "$ra"
    fi
    echo 1 > $P6
  fi
  nft -f "$C/nat.nft"
  docker_allow
}

# nat-down: remove the table and put forwarding back as it was before nat-up.
nat_down() {
  nft delete table inet srnet_seller 2>/dev/null || true
  docker_unallow
  if [ -f "$ST/forwarding.before" ]; then
    local k v
    while IFS='=' read -r k v; do
      case "$k:$v" in
        v4:[01]) echo "$v" > $P4;;
        v6:[01]) echo "$v" > $P6;;
        ra_*:?) [ -w "/proc/sys/net/ipv6/conf/${k#ra_}/accept_ra" ] && echo "$v" > "/proc/sys/net/ipv6/conf/${k#ra_}/accept_ra";;
      esac
    done < "$ST/forwarding.before"
    rm -f "$ST/forwarding.before"
    echo "forwarding restored: ipv4 $(rd $P4), ipv6 $(rd $P6)"
  fi
}

# check: everything the exit needs to pass buyers' traffic on. Prints each problem and
# how to fix it; returns 1 if any.
nat_check() {
  local bad=0 rules
  say() { echo "exit-seller: $*" >&2; bad=1; }
  [ "$(rd $P4)" = 1 ] || say "IPv4 forwarding is off (net.ipv4.ip_forward=$(rd $P4)); buyers' traffic would stop at this host. Fix: exit-seller.sh nat-up"
  if use_v6; then
    [ "$(rd $P6)" = 1 ] || say "IPv6 forwarding is off but --ipv6 is $IPV6. Fix: exit-seller.sh nat-up, or --ipv6 off"
  fi
  [ -n "$WAN_IF" ] && ip link show "$WAN_IF" >/dev/null 2>&1 || say "WAN interface '${WAN_IF}' not found. Fix: --wan IF"
  if ! rules=$(nft list table inet srnet_seller 2>/dev/null); then
    say "the NAT table (inet srnet_seller) is not loaded. Fix: exit-seller.sh nat-up"
  else
    grep -Eq "ip saddr $TUN_NET4 oifname \"?$WAN_IF\"? .*masquerade" <<<"$rules" ||
      say "no IPv4 masquerade for $TUN_NET4 out of $WAN_IF. Fix: exit-seller.sh nat-up"
    if use_v6; then
      grep -Eq "ip6 saddr $TUN_NET6 oifname \"?$WAN_IF\"? .*masquerade" <<<"$rules" ||
        say "no IPv6 masquerade for $TUN_NET6 out of $WAN_IF. Fix: exit-seller.sh nat-up"
    fi
  fi
  local ipt; ipt=$(command -v iptables || true)
  if [ -n "$ipt" ] && "$ipt" -S FORWARD 2>/dev/null | grep -q '^-P FORWARD DROP'; then
    "$ipt" -C DOCKER-USER -i "$TUN" -j ACCEPT 2>/dev/null ||
      say "iptables FORWARD policy is DROP and $TUN is not allowed through. Fix: exit-seller.sh nat-up"
  fi
  [ $bad = 0 ] && echo "forwarding and NAT OK (ipv4${IPV6:+, ipv6 $IPV6}) on $WAN_IF"
  return $bad
}

start_bg() { # name, cmd...
  local name=$1; shift
  if [ -f "$RUN/$name.pid" ] && kill -0 "$(cat "$RUN/$name.pid")" 2>/dev/null; then echo "$name already running"; return; fi
  nohup "$@" >> "$LOG/$name.log" 2>&1 &
  echo $! > "$RUN/$name.pid"
  echo "started $name (pid $!)"
}

router_bin() {
  SR=$PREFIX/bin/session-router; [ -x "$SR" ] || SR=$(command -v session-router || true)
  [ -n "$SR" ] || die "no session-router (install the patched debs or put it in $PREFIX/bin)"
}

# The exit router in the background (hosts without systemd). Same key file every time.
start_exit() {
  router_bin
  [ -f "$RUN/admissions.json" ] || echo '{"admissions":{}}' > "$RUN/admissions.json"
  start_bg exit env SR_EXIT_LEASE_ADMISSIONS="$RUN/admissions.json" SR_EXIT_LEASE_ISSUER_PUBKEY="$PUB" "$SR" "$C/exit.ini"
  for i in $(seq 30); do S=$(exit_sesh 2>/dev/null) && break; sleep 2; done
  [ -n "${S:-}" ] || die "exit router did not come up; see $LOG/exit.log"
  [ "$S" = "$(cat "$C/exit.sesh")" ] || die "router reports $S but its key file says $(cat "$C/exit.sesh")"
}

start_probe() {
  router_bin
  probe_ns_up
  start_bg probe ip netns exec "$PROBE_NS" "$SR" "$C/probe.ini"
}

# the watchdog command line; RESTART/RESTART_PROBE are shell commands
watchdog_cmd() {
  # shellcheck disable=SC2086
  echo "$SWAPD exits watchdog -probe-ns $PROBE_NS -probe-rpc ipc://$RUN/probe-rpc.sock -dns $DNS_IP -exit-sesh $1 $WATCHDOG_ARGS"
}

stop_bg() { # name
  local p; [ -f "$RUN/$1.pid" ] || return 0
  p=$(cat "$RUN/$1.pid")
  if kill "$p" 2>/dev/null; then
    for i in $(seq 40); do kill -0 "$p" 2>/dev/null || break; sleep 0.5; done
    kill -0 "$p" 2>/dev/null && kill -9 "$p" 2>/dev/null
    echo "stopped $1"
  fi
  rm -f "$RUN/$1.pid"
}

case "$CMD" in
  render) render;;
  install)
    need_root
    . /etc/os-release; [ "${VERSION_ID:-}" = 24.04 ] || echo "warning: built for Ubuntu 24.04" >&2
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nftables unbound dns-root-data iproute2 python3
    (cd "$DEBS" && sha256sum -c SHA256SUMS --ignore-missing) || die "patched debs fail their SHA-256 check"
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$DEBS"/session-router-bin_*.deb "$DEBS"/libsessionrouter*.deb
    systemctl disable --now unbound 2>/dev/null || true
    id srexit >/dev/null 2>&1 || useradd --system --home "$PREFIX" --shell /usr/sbin/nologin srexit
    render
    [ -x "$SWAPD" ] || die "copy the swapd binary to $SWAPD first"
    PUB=$(issuer_pub); router_key; probe_key; render >/dev/null
    [ -f "$DATA/root.key" ] || cp /usr/share/dns/root.key "$DATA/root.key"
    [ "$HERE/exit-seller.sh" -ef "$PREFIX/bin/exit-seller.sh" ] || install -m 755 "$HERE/exit-seller.sh" "$PREFIX/bin/exit-seller.sh"
    # forwarding survives a reboot; uninstall removes this file
    { echo "# exit seller kit"; echo "net.ipv4.ip_forward = 1"; use_v6 && echo "net.ipv6.conf.all.forwarding = 1"; true; } > "$SYSCTL_FILE"
    chown -R srexit:srexit "$DATA" "$RUN" "$LOG"; chown srexit:srexit "$C" "$C/exit.key" "$C/issuer.key"; chmod 600 "$C/exit.key" "$C/issuer.key"
    cat > $UNIT_DIR/srnet-exit-nat.service <<U
[Unit]
Description=Exit seller kit: forwarding rules
Before=srnet-exit.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$PREFIX/bin/exit-seller.sh nat-up --prefix $PREFIX
ExecStop=$PREFIX/bin/exit-seller.sh nat-down --prefix $PREFIX
[Install]
WantedBy=multi-user.target
U
    cat > $UNIT_DIR/srnet-exit.service <<U
[Unit]
Description=Exit seller kit: lease-gated Session Router exit (patched 1.1.1)
After=network-online.target srnet-exit-nat.service
Requires=srnet-exit-nat.service
Wants=network-online.target
[Service]
User=srexit
Group=srexit
Environment=SR_EXIT_LEASE_ADMISSIONS=$RUN/admissions.json
Environment=SR_EXIT_LEASE_ISSUER_PUBKEY=$PUB
# refuse to start an exit that can't pass traffic on
ExecStartPre=+$PREFIX/bin/exit-seller.sh check --prefix $PREFIX
ExecStart=/usr/bin/session-router $C/exit.ini
Restart=on-failure
RestartSec=5
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes
[Install]
WantedBy=multi-user.target
U
    cat > $UNIT_DIR/srnet-exit-dns.service <<U
[Unit]
Description=Exit seller kit: exit resolver (tunnel buyers only, no logs)
After=network-online.target
[Service]
ExecStartPre=-/usr/sbin/ip addr add $DNS_IP/32 dev lo
ExecStart=$UNBOUND -d -p -c $C/unbound.conf
Restart=on-failure
AmbientCapabilities=CAP_NET_BIND_SERVICE
[Install]
WantedBy=multi-user.target
U
    cat > $UNIT_DIR/srnet-exit-watcher.service <<U
[Unit]
Description=Exit seller kit: admit buyers from ExitLease purchases
After=srnet-exit.service
[Service]
User=srexit
ExecStart=/bin/sh -c 'exec $SWAPD exits watch -config $C/swapd.json -id $LISTING -exit-sesh \$(cat $C/exit.sesh) -issuer-key $C/issuer.key -store $RUN/admissions.json -state $RUN/watch-state.json'
Restart=always
RestartSec=10
NoNewPrivileges=yes
[Install]
WantedBy=multi-user.target
U
    cat > $UNIT_DIR/srnet-exit-probe.service <<U
[Unit]
Description=Exit seller kit: watchdog probe router (namespace $PROBE_NS)
After=srnet-exit-nat.service
[Service]
ExecStartPre=$PREFIX/bin/exit-seller.sh probe-ns-up --prefix $PREFIX
ExecStart=/usr/sbin/ip netns exec $PROBE_NS /usr/bin/session-router $C/probe.ini
Restart=always
RestartSec=10
[Install]
WantedBy=multi-user.target
U
    cat > $UNIT_DIR/srnet-exit-watchdog.service <<U
[Unit]
Description=Exit seller kit: probe the exit end to end, restart it when it stops answering
After=srnet-exit.service srnet-exit-probe.service
Wants=srnet-exit-probe.service
[Service]
ExecStart=$(watchdog_cmd "$(cat "$C/exit.sesh")") -restart "systemctl restart srnet-exit" -restart-probe "systemctl restart srnet-exit-probe"
Restart=always
RestartSec=30
[Install]
WantedBy=multi-user.target
U
    systemctl daemon-reload
    systemctl enable srnet-exit-nat srnet-exit-dns srnet-exit
    systemctl restart srnet-exit-nat
    nat_check || die "forwarding/NAT check failed; the exit was not started"
    systemctl start srnet-exit-dns srnet-exit
    for i in $(seq 30); do S=$(exit_sesh 2>/dev/null) && break; sleep 2; done
    [ -n "${S:-}" ] || die "exit router did not come up; see journalctl -u srnet-exit"
    [ "$S" = "$(cat "$C/exit.sesh")" ] || die "router reports $S but its key file says $(cat "$C/exit.sesh")"
    [ "$LISTING" != 0 ] && systemctl enable --now srnet-exit-watcher
    if [ "$WATCHDOG" = on ]; then
      systemctl enable --now srnet-exit-probe srnet-exit-watchdog
    else
      systemctl disable --now srnet-exit-watchdog srnet-exit-probe 2>/dev/null || true
    fi
    echo "exit .sesh: $S"
    echo "next: register it (swapd registry register -tag exit), set a price (swapd exits offer),"
    echo "      then re-run with --listing N to start the watcher."
    ;;
  up)
    need_root
    render
    [ -x "$SWAPD" ] || die "no swapd binary at $SWAPD (use --swapd)"
    command -v session-router >/dev/null || [ -x "$PREFIX/bin/session-router" ] || die "no session-router (install the patched debs or put it in $PREFIX/bin)"
    PUB=$(issuer_pub); router_key; probe_key; render >/dev/null
    [ -n "$DNS_FORWARD" ] || [ -f "$DATA/root.key" ] || cp /usr/share/dns/root.key "$DATA/root.key"
    nat_up
    nat_check || die "forwarding/NAT check failed; not starting the exit"
    ip addr show dev lo | grep -q " $DNS_IP/" || ip addr add "$DNS_IP/32" dev lo
    start_bg dns $UNBOUND -d -p -c "$C/unbound.conf"
    start_exit
    echo "exit .sesh: $S   issuer: $PUB"
    if [ "$LISTING" != 0 ]; then
      start_bg watcher "$SWAPD" exits watch -config "$C/swapd.json" -id "$LISTING" -exit-sesh "$S" \
        -issuer-key "$C/issuer.key" -store "$RUN/admissions.json" -state "$RUN/watch-state.json"
    fi
    if [ "$WATCHDOG" = on ]; then
      start_probe
      # shellcheck disable=SC2046
      start_bg watchdog $(watchdog_cmd "$S") -restart "$HERE/exit-seller.sh restart-exit --prefix $PREFIX" \
        -restart-probe "$HERE/exit-seller.sh restart-probe --prefix $PREFIX"
    fi
    ;;
  restart-exit)
    need_root
    PUB=$(issuer_pub)
    echo "$(date '+%F %T %Z') restart-exit: stopping the exit router" >> "$LOG/exit.log"
    stop_bg exit
    start_exit
    echo "exit router restarted: $S";;
  restart-probe)
    need_root
    stop_bg probe
    start_probe;;
  probe-ns-up) need_root; probe_ns_up;;
  down)
    need_root
    for n in watchdog probe watcher exit dns; do stop_bg $n; done
    probe_ns_down
    nat_down
    ip addr del "$DNS_IP/32" dev lo 2>/dev/null || true
    ;;
  uninstall)
    need_root
    for u in srnet-exit-watchdog srnet-exit-probe srnet-exit-watcher srnet-exit srnet-exit-dns srnet-exit-nat; do
      systemctl disable --now "$u" 2>/dev/null || true
      rm -f "$UNIT_DIR/$u.service"
    done
    systemctl daemon-reload 2>/dev/null || true
    probe_ns_down
    nat_down
    rm -f "$SYSCTL_FILE"
    ip addr del "$DNS_IP/32" dev lo 2>/dev/null || true
    echo "removed the units, NAT and $SYSCTL_FILE; keys and data are still in $PREFIX"
    ;;
  nat-up) need_root; render >/dev/null; nat_up; nat_check;;
  nat-down) need_root; nat_down;;
  check) nat_check;;
  allow)
    if [ ${#ARGS[@]} -eq 0 ] && [ ${#UNLIMITED_ADD[@]} -eq 0 ]; then list_allowed; exit 0; fi
    [ ${#ARGS[@]} -le 1 ] || die "usage: exit-seller.sh allow <addr>.sesh"
    [ ${#ARGS[@]} -eq 1 ] && allow_one "${ARGS[0]}"
    # --unlimited addresses are merged by render (via apply_allowed)
    apply_allowed
    if [ ${#ARGS[@]} -eq 0 ]; then list_allowed; fi
    ;;
  disallow)
    [ ${#ARGS[@]} -eq 1 ] || die "usage: exit-seller.sh disallow <addr>.sesh"
    disallow_one "${ARGS[0]}"
    apply_allowed
    ;;
  allowed) list_allowed;;
  status)
    for n in dns exit watcher probe watchdog; do
      if [ -f "$RUN/$n.pid" ] && kill -0 "$(cat "$RUN/$n.pid")" 2>/dev/null; then echo "$n: running"; else echo "$n: stopped"; fi
    done
    [ -f "$C/exit.sesh" ] && echo "exit: $(cat "$C/exit.sesh")"
    [ "$(id -u)" = 0 ] && { nat_check 2>&1 || true; }
    list_allowed
    [ -f "$RUN/admissions.json" ] && python3 -c "import json,sys,time;a=json.load(open(sys.argv[1]))['admissions'];[print('admitted',r['client_sesh'],'until',time.strftime('%Y-%m-%d %H:%M %Z',time.localtime(r['exp']))) for r in a.values()]" "$RUN/admissions.json" 2>/dev/null || true
    ;;
  *) awk 'NR>1 && /^set -euo/{exit} NR>1' "$0"; exit 2;;
esac
