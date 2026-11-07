package routeros

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// startupDiag runs a one-off, read-only L3HW health check after the initial
// sync. Every failure is logged and ignored; it never affects routing.
func (r *RouterOS) startupDiag(defGw netip.Addr, static []StaticGroup) {
	l := r.logger.Named("l3hw")

	// 1. switch chip offloading state
	sw, err := r.cl.print("/interface/ethernet/switch/print")
	if err != nil {
		l.Warn("switch print failed", zap.Error(err))
	}
	// Devices may have several switch chips (e.g. CRS504: the 98DX4310 data
	// plane plus a small Atheros chip for the management port). Only warn when
	// no chip at all has offloading on; the per-gateway check below tells
	// whether the chip that matters is enabled.
	anyOn := false
	for _, s := range sw {
		v := s.Map["l3-hw-offloading"]
		fields := []zap.Field{zap.String("switch", s.Map["name"]), zap.String("type", s.Map["type"]), zap.String("l3-hw-offloading", v)}
		if e := s.Map["l3hw-error"]; e != "" {
			fields = append(fields, zap.String("l3hw-error", e))
		}
		if isTrue(v) {
			anyOn = true
		}
		l.Info("switch chip", fields...)
	}
	if len(sw) > 0 && !anyOn {
		l.Warn("no switch chip has l3-hw-offloading=yes; all routes will be forwarded by CPU")
	}

	// 2. gateway reachability / interface offloading
	gws := map[netip.Addr]bool{defGw: true}
	for _, g := range static {
		if a, err := parseGateway(g.Gateway); err == nil {
			gws[a] = true
		}
	}
	addrs, err := r.cl.print("/ip/address/print", "=.proplist=address,interface,disabled")
	if err != nil {
		l.Warn("ip address print failed", zap.Error(err))
	}
	for gw := range gws {
		iface := ""
		for _, a := range addrs {
			if isTrue(a.Map["disabled"]) {
				continue
			}
			if p, err := netip.ParsePrefix(a.Map["address"]); err == nil && p.Contains(gw) {
				iface = a.Map["interface"]
				break
			}
		}
		if iface == "" {
			l.Warn("gateway is not in any connected network of the router; routes via it cannot be resolved",
				zap.Stringer("gateway", gw))
			continue
		}
		state := r.ifaceL3HW(iface, 0)
		f := []zap.Field{zap.Stringer("gateway", gw), zap.String("interface", iface), zap.String("l3hw", state)}
		switch state {
		case "yes":
			l.Info("gateway interface offload ok", f...)
		case "unknown":
			l.Info("gateway interface offload state could not be determined", f...)
		default:
			l.Warn("gateway interface has L3HW disabled; traffic via this gateway goes through CPU", f...)
		}
	}

	// 3. settings (informational)
	for _, cmd := range []string{
		"/interface/ethernet/switch/l3hw-settings/print",
		"/interface/ethernet/switch/l3hw-settings/advanced/print",
	} {
		re, err := r.cl.print(cmd)
		if err != nil {
			l.Debug("print failed", zap.String("cmd", cmd), zap.Error(err))
			continue
		}
		for _, s := range re {
			l.Info(strings.TrimPrefix(cmd, "/interface/ethernet/switch/"), zap.String("values", flatten(s.Map)))
		}
	}

	// 4. monitor
	mon, err := r.l3hwMonitor()
	if err != nil {
		l.Warn("l3hw monitor failed", zap.Error(err))
		return
	}
	l.Info("l3hw monitor", zap.String("values", flatten(mon)))
	if v := atoi(mon["ipv4-shortest-hw-prefix"]); v != 0 {
		l.Warn("partial offloading: hardware route table is full, prefixes shorter than this are on CPU",
			zap.Int("ipv4-shortest-hw-prefix", v))
	}
	if v := atoi(mon["route-queue-size"]); v > 0 {
		l.Warn("switch driver route queue is not empty", zap.Int("route-queue-size", v))
	}
	if capv, use := atoi(mon["nexthop-cap"]), atoi(mon["nexthop-usage"]); capv > 0 && use*10 >= capv*9 {
		l.Warn("nexthop table nearly full", zap.Int("nexthop-usage", use), zap.Int("nexthop-cap", capv))
	}
}

// ifaceL3HW best-effort resolves the l3-hw-offloading state of an interface:
// switch port -> direct; vlan -> its own flag (7.21+) or its parent; bridge ->
// all member switch ports must be enabled. Returns "yes", "no" or "unknown".
func (r *RouterOS) ifaceL3HW(name string, depth int) string {
	if depth > 3 {
		return "unknown"
	}
	if re, err := r.cl.print("/interface/ethernet/switch/port/print", "?name="+name, "=.proplist=l3-hw-offloading"); err == nil && len(re) > 0 {
		if v, ok := re[0].Map["l3-hw-offloading"]; ok {
			return yesNo(v)
		}
	}
	if re, err := r.cl.print("/interface/vlan/print", "?name="+name, "=.proplist=interface,l3-hw-offloading"); err == nil && len(re) > 0 {
		if v, ok := re[0].Map["l3-hw-offloading"]; ok && !isTrue(v) {
			return "no"
		}
		if parent := re[0].Map["interface"]; parent != "" {
			return r.ifaceL3HW(parent, depth+1)
		}
	}
	if re, err := r.cl.print("/interface/bridge/port/print", "?bridge="+name, "=.proplist=interface"); err == nil && len(re) > 0 {
		result := "yes"
		for _, p := range re {
			switch r.ifaceL3HW(p.Map["interface"], depth+1) {
			case "no":
				return "no"
			case "unknown":
				result = "unknown"
			}
		}
		return result
	}
	return "unknown"
}

// l3hwMonitor returns one snapshot of /interface/ethernet/switch/l3hw-settings/monitor.
func (r *RouterOS) l3hwMonitor() (map[string]string, error) {
	re, err := r.cl.print("/interface/ethernet/switch/l3hw-settings/monitor", "=once=")
	if err != nil {
		return nil, err
	}
	if len(re) == 0 {
		return nil, fmt.Errorf("monitor returned no data")
	}
	return re[0].Map, nil
}

func isTrue(v string) bool { return v == "true" || v == "yes" }

func yesNo(v string) string {
	if isTrue(v) {
		return "yes"
	}
	return "no"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func flatten(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(m[k])
	}
	return sb.String()
}
