#!/usr/bin/env bash
# Forwarding/NAT test for exit-seller.sh in throwaway network namespaces (root).
# Nothing on the host changes: forwarding and nftables are per namespace.
#
#   [client ns] 10.193.0.2 --veth "sr-tun-seller"--> [seller ns] --wan0 192.0.2.10--> [net ns] 192.0.2.1
#
# Checks: check fails from clean; nat-up turns forwarding on and NATs web traffic (the
# server sees the seller's address); port 22 stays closed; nat-up twice gives the same
# ruleset; nat-down/uninstall put everything back; IPv6 (auto) gets its own masquerade.
set -euo pipefail
KIT=$(cd "$(dirname "$0")/.." && pwd)/exit-seller.sh
S=srxnat-s; CL=srxnat-c; NET=srxnat-n; PROBE=srxnat-p # never the live exitprobe
T=$(mktemp -d); PFX=$T/prefix
pass=0; fail=0
ok()  { echo "PASS  $*"; pass=$((pass+1)); }
bad() { echo "FAIL  $*"; fail=$((fail+1)); }
cleanup() {
  ip netns pids $NET 2>/dev/null | xargs -r kill 2>/dev/null || true
  for n in $S $CL $NET $PROBE; do ip netns del $n 2>/dev/null || true; done
  rm -rf "$T" "/etc/netns/$PROBE"
}
trap cleanup EXIT
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 2; }

for n in $S $CL $NET; do ip netns add $n; ip -n $n link set lo up; done
ip link add tunA netns $S type veth peer name tunB netns $CL
ip -n $S link set tunA name sr-tun-seller
ip -n $S addr add 10.193.0.1/16 dev sr-tun-seller; ip -n $S link set sr-tun-seller up
ip -n $CL addr add 10.193.0.2/16 dev tunB; ip -n $CL link set tunB up
ip -n $CL route add default via 10.193.0.1
ip link add wan0 netns $S type veth peer name up0 netns $NET
ip -n $S addr add 192.0.2.10/24 dev wan0; ip -n $S link set wan0 up
ip -n $NET addr add 192.0.2.1/24 dev up0; ip -n $NET link set up0 up
ip -n $S route add default via 192.0.2.1
# the "internet": answers on 443 and 22 and reports who connected
cat > $T/srv.py <<'PY'
import socket, threading, sys
def serve(port):
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0", port)); s.listen(8)
    while True:
        c, a = s.accept(); c.sendall(a[0].encode()); c.close()
for p in (443, 22): threading.Thread(target=serve, args=(p,), daemon=True).start()
threading.Event().wait()
PY
ip netns exec $NET python3 $T/srv.py & sleep 0.5
peer() { ip netns exec $CL python3 -c 'import socket,sys
try:
  s=socket.create_connection(("192.0.2.1",int(sys.argv[1])),timeout=2); print(s.recv(64).decode())
except Exception as e: print("blocked")' "$1"; }
kit() { ip netns exec $S env PATH="$T/bin:$PATH" UNIT_DIR=$T/units SYSCTL_FILE=$T/sysctl.conf "$KIT" "$@" --prefix "$PFX" --probe-ns $PROBE; }
fwd() { ip netns exec $S cat /proc/sys/net/ipv4/ip_forward; }
fwd6() { ip netns exec $S cat /proc/sys/net/ipv6/conf/all/forwarding; }

# 1. clean namespace: forwarding off, no table, check refuses with the reason
echo 0 | ip netns exec $S tee /proc/sys/net/ipv4/ip_forward >/dev/null
out=$(kit check --wan wan0 --ipv6 off 2>&1) && bad "check passed on a clean host" || {
  grep -q "IPv4 forwarding is off" <<<"$out" && grep -q "NAT table" <<<"$out" && ok "check refuses from clean: $(head -1 <<<"$out" | cut -c1-90)…" || bad "check message: $out"; }
[ "$(peer 443)" = blocked ] && ok "no forwarding before nat-up" || bad "traffic passed before nat-up"

# 2. nat-up from clean
kit nat-up --wan wan0 --ipv6 off >/dev/null
[ "$(fwd)" = 1 ] && ok "nat-up turned ip_forward on" || bad "ip_forward=$(fwd)"
kit check >/dev/null 2>&1 && ok "check passes after nat-up (settings read back from kit.env)" || bad "check after nat-up"
r=$(peer 443); [ "$r" = 192.0.2.10 ] && ok "tcp/443 from a buyer reaches the internet as the seller ($r)" || bad "443 got '$r'"
r=$(peer 22); [ "$r" = blocked ] && ok "tcp/22 is not forwarded" || bad "22 got '$r'"
rs() { ip netns exec $S nft list ruleset | sed 's/packets [0-9]* bytes [0-9]*//'; }
A=$(rs)

# 3. idempotent
kit nat-up >/dev/null; kit nat-up >/dev/null
[ "$(ip netns exec $S nft list tables | grep -c srnet_seller)" = 1 ] && [ "$(ip netns exec $S nft list ruleset | grep -c "10.193.0.0/16.*masquerade")" = 1 ] \
  && ok "nat-up three times: one table, one masquerade rule" || bad "duplicate rules after repeat nat-up"
[ "$(grep -c . $PFX/state/forwarding.before)" = 2 ] && grep -q '^v4=0$' $PFX/state/forwarding.before \
  && ok "the pre-kit forwarding value (0) is kept across repeats" || bad "forwarding.before: $(cat $PFX/state/forwarding.before)"
[ "$A" = "$(rs)" ] \
  && ok "ruleset unchanged by repeats" || bad "ruleset changed"

# 4. a missing piece is reported
echo 0 | ip netns exec $S tee /proc/sys/net/ipv4/ip_forward >/dev/null
out=$(kit check 2>&1) && bad "check passed with forwarding off" || { grep -q "IPv4 forwarding is off" <<<"$out" && ok "check catches forwarding switched off later" || bad "$out"; }
ip netns exec $S nft delete rule inet srnet_seller postrouting_nat handle $(ip netns exec $S nft -a list chain inet srnet_seller postrouting_nat | awk '/10.193.0.0\/16.*masquerade/{print $NF}')
out=$(kit check 2>&1) || true; grep -q "no IPv4 masquerade" <<<"$out" && ok "check catches a missing masquerade rule" || bad "$out"
kit nat-up >/dev/null; kit check >/dev/null 2>&1 && ok "nat-up repairs both" || bad "nat-up did not repair"

# 5. nat-down restores
kit nat-down >/dev/null
[ "$(fwd)" = 0 ] && ! ip netns exec $S nft list table inet srnet_seller >/dev/null 2>&1 && [ ! -f $PFX/state/forwarding.before ] \
  && ok "nat-down: table gone, ip_forward back to 0" || bad "nat-down left fwd=$(fwd)"
[ "$(peer 443)" = blocked ] && ok "no forwarding after nat-down" || bad "traffic still forwarded"

# 6. IPv6 auto: on when there is an IPv6 default route
ip -n $S addr add 2001:db8:1::10/64 dev wan0 nodad; ip -n $NET addr add 2001:db8:1::1/64 dev up0 nodad
ip -n $S route add default via 2001:db8:1::1
echo 1 | ip netns exec $S tee /proc/sys/net/ipv6/conf/wan0/accept_ra >/dev/null
kit nat-up --ipv6 auto >/dev/null
ip netns exec $S nft list table inet srnet_seller | grep -q "ip6 saddr fd2e:7365:7369::/64 oifname \"wan0\".*masquerade" && [ "$(fwd6)" = 1 ] \
  && ok "ipv6 auto: v6 masquerade + v6 forwarding" || bad "ipv6 auto"
[ "$(ip netns exec $S cat /proc/sys/net/ipv6/conf/wan0/accept_ra)" = 2 ] && ok "accept_ra=2 on the WAN keeps its router advertisements" || bad "accept_ra"
kit check >/dev/null 2>&1 && ok "check passes with ipv6" || bad "check with ipv6"

# 7. uninstall (systemctl stubbed) removes units, sysctl file, NAT; restores forwarding
mkdir -p $T/bin $T/units; printf '#!/bin/sh\necho "systemctl $*" >> %s/systemctl.log\n' $T > $T/bin/systemctl; chmod +x $T/bin/systemctl
for u in srnet-exit srnet-exit-nat srnet-exit-dns srnet-exit-watcher; do touch $T/units/$u.service; done
echo "net.ipv4.ip_forward = 1" > $T/sysctl.conf
kit uninstall >/dev/null
[ -z "$(ls $T/units)" ] && [ ! -f $T/sysctl.conf ] && [ "$(fwd)" = 0 ] && [ "$(fwd6)" = 0 ] \
  && [ "$(ip netns exec $S cat /proc/sys/net/ipv6/conf/wan0/accept_ra)" = 1 ] \
  && ! ip netns exec $S nft list table inet srnet_seller >/dev/null 2>&1 \
  && ok "uninstall: units, sysctl file and NAT removed; forwarding and accept_ra restored" || bad "uninstall left something: $(ls $T/units) fwd=$(fwd)/$(fwd6)"
grep -q "disable --now srnet-exit-nat" $T/systemctl.log && ok "uninstall stopped and disabled the units" || bad "systemctl calls"

echo "nat-test: $pass passed, $fail failed"
[ $fail = 0 ]
