package relay

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"graphrelay/internal/store"
)

// Matcher finds the host rule for a client IP. When several rules match,
// the most specific wins: exact IP / DNS name, then the longest CIDR prefix.
type Matcher struct {
	store *store.Store

	mu    sync.Mutex
	cache map[string]dnsEntry
}

type dnsEntry struct {
	addrs   []netip.Addr
	expires time.Time
}

const dnsCacheTTL = 2 * time.Minute

func NewMatcher(s *store.Store) *Matcher {
	return &Matcher{store: s, cache: map[string]dnsEntry{}}
}

func (m *Matcher) Match(ctx context.Context, ip netip.Addr) (*store.Host, error) {
	all, err := m.MatchAll(ctx, ip)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[0], nil
}

// MatchAll returns every enabled host rule that matches ip, most specific first (ties keep the
// rules' name order). A login is looked up among all of them, so several rules for the same
// address can be told apart by the login the client uses.
func (m *Matcher) MatchAll(ctx context.Context, ip netip.Addr) ([]store.Host, error) {
	hosts, err := m.store.ListHosts()
	if err != nil {
		return nil, err
	}
	ip = ip.Unmap()
	type scored struct {
		h     store.Host
		score int
	}
	var found []scored
	for _, h := range hosts {
		if !h.Enabled {
			continue
		}
		if score := m.score(ctx, h.Match, ip); score >= 0 {
			found = append(found, scored{h, score})
		}
	}
	slices.SortStableFunc(found, func(a, b scored) int { return b.score - a.score })
	out := make([]store.Host, len(found))
	for i, f := range found {
		out[i] = f.h
	}
	return out, nil
}

// score returns -1 for no match, otherwise a specificity value. For a rule
// with several entries it is the best score of any entry.
func (m *Matcher) score(ctx context.Context, match string, ip netip.Addr) int {
	best := -1
	for _, entry := range store.SplitMatch(match) {
		if s := m.scoreOne(ctx, entry, ip); s > best {
			best = s
		}
	}
	return best
}

func (m *Matcher) scoreOne(ctx context.Context, match string, ip netip.Addr) int {
	if a, err := netip.ParseAddr(match); err == nil {
		if a.Unmap() == ip {
			return 1000
		}
		return -1
	}
	if p, err := netip.ParsePrefix(match); err == nil {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-unmapBits(p.Addr()))
		if p.Contains(ip) {
			return p.Bits()
		}
		return -1
	}
	for _, a := range m.resolve(ctx, match) {
		if a == ip {
			return 999
		}
	}
	return -1
}

func unmapBits(a netip.Addr) int {
	if a.Is4In6() {
		return 96
	}
	return 0
}

// Resolve returns the cached addresses of a DNS host rule.
func (m *Matcher) Resolve(ctx context.Context, name string) []netip.Addr { return m.resolve(ctx, name) }

func (m *Matcher) resolve(ctx context.Context, name string) []netip.Addr {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	m.mu.Lock()
	e, ok := m.cache[name]
	m.mu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.addrs
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
	if err != nil && ok {
		return e.addrs // keep serving the last good answer if DNS hiccups
	}
	addrs := make([]netip.Addr, 0, len(ips))
	for _, a := range ips {
		addrs = append(addrs, a.Unmap())
	}
	m.mu.Lock()
	m.cache[name] = dnsEntry{addrs: addrs, expires: time.Now().Add(dnsCacheTTL)}
	m.mu.Unlock()
	return addrs
}

// MaxMatchEntries limits how many addresses one host rule may list.
const MaxMatchEntries = 256

// ValidateMatch checks the syntax of a host match value and normalises it.
// The value is one entry or a list separated by commas, semicolons or
// whitespace; duplicates are dropped and the result is joined with ", ".
func ValidateMatch(s string) (string, bool) {
	entries := store.SplitMatch(s)
	if len(entries) == 0 || len(entries) > MaxMatchEntries {
		return "", false
	}
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		norm, ok := validateEntry(e)
		if !ok {
			return "", false
		}
		if !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	return strings.Join(out, ", "), true
}

func validateEntry(s string) (string, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().String(), true
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked().String(), true
	}
	// DNS name: letters, digits, hyphens and dots.
	if s == "" || len(s) > 253 {
		return "", false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if label == "" || len(label) > 63 {
			return "", false
		}
		for _, r := range label {
			if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return "", false
			}
		}
	}
	return strings.ToLower(s), true
}
