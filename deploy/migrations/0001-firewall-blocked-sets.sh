#!/bin/bash
# Host migration 0001: count firewall-dropped SMTP sources.
#
# Releases with the Activity page read the nftables sets smtp_blocked4 / smtp_blocked6. A host
# whose /etc/nftables.conf was written by an earlier harden-host.sh has no such sets, and nothing
# else would add them. This adds the two sets and the rule that fills them.
#
# Only a ruleset written by harden-host.sh is touched; a hand-made firewall is left alone.
# Safe to run again: it does nothing when the sets are already there.
set -euo pipefail

CONF=/etc/nftables.conf

if [ ! -e "$CONF" ] || ! grep -q 'harden-host.sh' "$CONF"; then
  echo "no firewall ruleset written by harden-host.sh: nothing to do"
  exit 0
fi
if grep -q 'smtp_blocked4' "$CONF"; then
  echo "blocked-source sets already present: nothing to do"
  exit 0
fi
if ! grep -q 'set smtp6 ' "$CONF" || ! grep -q 'ip6 saddr @smtp6 tcp dport' "$CONF"; then
  echo "ruleset layout not recognised: leaving it alone (run harden-host.sh to get the current ruleset)"
  exit 0
fi

new=$(mktemp)
trap 'rm -f "$new"' EXIT
awk '
  { print }
  /^[[:space:]]*set smtp6 / {
    print "  set smtp_blocked4 { type ipv4_addr; size 4096; flags dynamic,timeout; counter; timeout 1d; }"
    print "  set smtp_blocked6 { type ipv6_addr; size 4096; flags dynamic,timeout; counter; timeout 1d; }"
  }
  /ip6 saddr @smtp6 tcp dport/ {
    print "    tcp dport { 25, 465, 587 } ct state new update @smtp_blocked4 { ip saddr }"
    print "    tcp dport { 25, 465, 587 } ct state new update @smtp_blocked6 { ip6 saddr }"
  }
' "$CONF" > "$new"

# Never install a ruleset nft would reject.
nft -c -f "$new"
cp -a "$CONF" "$CONF.before-0001"
cat "$new" > "$CONF"
if systemctl is-active -q nftables; then
  nft -f "$CONF"
  # Loading the ruleset emptied the allowlist sets; fill them again.
  if [ -x /usr/local/sbin/graphrelay-fw-sync ]; then
    /usr/local/sbin/graphrelay-fw-sync
  fi
fi
echo "added smtp_blocked4/smtp_blocked6 to $CONF (previous file: $CONF.before-0001)"
