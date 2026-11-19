#!/usr/bin/env bash
# Non-root render/allow tests for exit-seller.sh. No host networking is changed.
set -euo pipefail
KIT=$(cd "$(dirname "$0")/.." && pwd)/exit-seller.sh
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
pass=0; fail=0
ok()  { echo "PASS  $*"; pass=$((pass+1)); }
bad() { echo "FAIL  $*"; fail=$((fail+1)); }
A=yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy.sesh
B=zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.sesh

bash -n "$KIT" && ok "bash -n" || bad "bash -n"

"$KIT" render --prefix "$T" --wan eth0 --ipv6 off >/dev/null
grep -q 'th dport 53 drop' "$T/config/nat.nft" && bad "still drops all forwarded DNS" || ok "no blanket DNS drop"
grep -q 'dnsfwd_rl4' "$T/config/nat.nft" && grep -q 'limit rate over 100/second burst 200 packets' "$T/config/nat.nft" \
  && ok "forwarded DNS is rate-limited 100/s burst 200" || bad "missing DNS forward rate limit"
grep -q 'tcp dport { 80, 443 }' "$T/config/nat.nft" && ok "web 80/443 still forwarded" || bad "web rules"
grep -q 'web_rl4' "$T/config/nat.nft" && grep -q 'limit rate over 50/second burst 100 packets' "$T/config/nat.nft" \
  && grep -q 'ct state new update @web_rl4' "$T/config/nat.nft" \
  && ok "web 80/443 new connections are rate-limited 50/s burst 100" || bad "missing web rate limit"
grep -q 'tcp dport { 80, 443 } ip saddr @unlimited4 counter accept' "$T/config/nat.nft" \
  && ok "unlimited4 is exempt from the web rate limit" || bad "no web unlimited exemption"
grep -q "ip daddr 10.195.0.53" "$T/config/nat.nft" && ok "local resolver still accepted on the tunnel" || bad "local DNS"

"$KIT" allow --prefix "$T" --wan eth0 --ipv6 off "$A" >/dev/null
grep -q "auth-whitelist=$A" "$T/config/exit.ini" && grep -q "mapaddr=$A:10.193.255.254" "$T/config/exit.ini" \
  && ok "allow writes whitelist and mapaddr" || bad "exit.ini after allow"
grep -q '10.193.255.254' "$T/config/nat.nft" && ok "allow adds the pinned IP to unlimited4" || bad "unlimited4 after allow"
"$KIT" allow --prefix "$T" --wan eth0 --ipv6 off --unlimited "$B" >/dev/null
grep -q "auth-whitelist=$B" "$T/config/exit.ini" && grep -q "10.193.255.253" "$T/config/allowed" \
  && ok "--unlimited / second allow keeps a stable new pin" || bad "second allow"
# re-allow A does not change its IP
old=$(awk -v a="$A" '$1==a{print $2}' "$T/config/allowed")
"$KIT" allow --prefix "$T" --wan eth0 --ipv6 off "$A" >/dev/null
new=$(awk -v a="$A" '$1==a{print $2}' "$T/config/allowed")
[ "$old" = "$new" ] && [ "$old" = "10.193.255.254" ] && ok "re-allow keeps the same pinned IP" || bad "pin changed: $old -> $new"
"$KIT" disallow --prefix "$T" --wan eth0 --ipv6 off "$A" >/dev/null
grep -q "$A" "$T/config/allowed" && bad "A still in allowed" || ok "disallow removes A"
grep -q "auth-whitelist=$A" "$T/config/exit.ini" && bad "A still in exit.ini" || ok "disallow rewrites exit.ini"
grep -q "auth-whitelist=$B" "$T/config/exit.ini" && ok "disallow leaves the other address" || bad "B was removed"
! "$KIT" allow --prefix "$T" not-a-sesh >/dev/null 2>&1 && ok "invalid address is rejected" || bad "invalid address accepted"

# nft syntax, ipv4-only and ipv6-on
if [ -x /usr/sbin/nft ]; then
  unshare -rn -- /usr/sbin/nft -c -f "$T/config/nat.nft" && ok "nft -c accepts the ipv4-only table" || bad "nft -c ipv4"
  "$KIT" render --prefix "$T" --wan eth0 --ipv6 on >/dev/null
  grep -q dnsfwd_rl6 "$T/config/nat.nft" && ok "ipv6 on: forwarded DNS rate-limit for v6" || bad "no dnsfwd_rl6"
  grep -q web_rl6 "$T/config/nat.nft" && grep -q 'update @web_rl6' "$T/config/nat.nft" \
    && ok "ipv6 on: web rate-limit for v6" || bad "no web_rl6"
  unshare -rn -- /usr/sbin/nft -c -f "$T/config/nat.nft" && ok "nft -c accepts the ipv6-on table" || bad "nft -c ipv6"
  unshare -rn -- /usr/sbin/nft -f "$T/config/nat.nft" && ok "nft -f loads the table in a netns" || bad "nft -f load"
else
  echo "skip nft checks: /usr/sbin/nft not found"
fi

echo "render-test: $pass passed, $fail failed"
[ $fail = 0 ]
