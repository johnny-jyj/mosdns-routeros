package routeros

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/IrineSistiana/mosdns/v5/pkg/utils"
	"go.uber.org/zap"
)

// loadPrefixes parses IPv4 addresses / CIDRs from inline strings and files.
// IPv6 entries are skipped (counted into one warning). Non-canonical IPv4
// prefixes are masked.
func loadPrefixes(logger *zap.Logger, ips []string, files []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	skipped := 0
	for i, s := range ips {
		p, v6, err := parsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("invalid ip #%d %q: %w", i, s, err)
		}
		if v6 {
			skipped++
			continue
		}
		out = append(out, p)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", f, err)
		}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		line := 0
		for sc.Scan() {
			line++
			s := strings.TrimSpace(sc.Text())
			s = utils.RemoveComment(s, "#")
			s = utils.RemoveComment(s, " ")
			if s == "" {
				continue
			}
			p, v6, err := parsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("%s line %d %q: %w", f, line, s, err)
			}
			if v6 {
				skipped++
				continue
			}
			out = append(out, p)
		}
	}
	if skipped > 0 {
		logger.Warn("skipped IPv6 entries in static routes (IPv4 only)", zap.Int("count", skipped))
	}
	return out, nil
}

// parsePrefix accepts "a.b.c.d" or "a.b.c.d/n". The bool is true for IPv6.
func parsePrefix(s string) (netip.Prefix, bool, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, false, err
		}
		if !p.Addr().Is4() {
			return netip.Prefix{}, true, nil
		}
		return p.Masked(), false, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, false, err
	}
	if !a.Is4() {
		return netip.Prefix{}, true, nil
	}
	return netip.PrefixFrom(a, 32), false, nil
}

// parseGateway validates a next-hop: IPv4 unicast host address.
func parseGateway(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid gateway %q: %w", s, err)
	}
	switch {
	case !a.Is4():
		return netip.Addr{}, fmt.Errorf("gateway must be IPv4: %q", s)
	case a.IsUnspecified(), a.IsLoopback(), a.IsMulticast(), a.IsLinkLocalUnicast(),
		a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return netip.Addr{}, fmt.Errorf("gateway must be a unicast host address: %q", s)
	}
	return a, nil
}
