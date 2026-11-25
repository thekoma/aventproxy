package webrtc

import (
	"net"
	"strings"

	"github.com/pion/ice/v4"
)

// LAN-only WebRTC: media between the bridge and a camera may only travel over
// the local network. Three layers make that hold without any firewall:
//
//   - no STUN or TURN servers, so the bridge gathers host candidates only and
//     never allocates a relay (the caller leaves ICEServers empty);
//   - the camera's signalled candidates, in its answer and trickled, go through
//     LANCandidateFor / FilterLANCandidates: host candidates on a private
//     address survive, a few that name the camera's own LAN address are
//     rewritten onto it (see LANCandidateFor), and the rest are dropped;
//   - the ICE agent gets IsLANAddress as its remote IP filter (NewServerAPI with
//     Filters.LANOnly), which pion applies to signalled candidates and to the
//     peer-reflexive ones it learns from incoming connectivity checks alike. A
//     check from a private address (the camera answering through the host's
//     NAT, or checking from an address it did not signal) still forms a pair;
//     one from a public address is ignored.

// IsLANAddress reports whether ip is an address that stays on the local
// network: RFC 1918 IPv4, IPv4/IPv6 link-local, or an IPv6 unique local
// address (fc00::/7). Loopback, CGNAT (100.64.0.0/10) and every global address
// are not.
func IsLANAddress(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// LANCandidateAllowed reports whether a remote ICE candidate line may be used
// on a LAN-only connection: a host candidate whose address is a literal LAN
// address, or a peer-reflexive one on a LAN address. Server-reflexive and relay
// candidates are refused whatever their address (they exist to cross a NAT or
// reach a TURN server), as are mDNS names, which cannot be checked, and
// anything that does not parse. The line may carry the "a=" and "candidate:"
// prefixes. typ is the candidate type for logging ("" when unparsable); it
// never includes the address.
func LANCandidateAllowed(line string) (allowed bool, typ string) {
	raw := strings.TrimSpace(line)
	raw = strings.TrimPrefix(raw, "a=")
	if raw == "" {
		return false, ""
	}
	cand, err := ice.UnmarshalCandidate(raw)
	if err != nil {
		return false, ""
	}
	typ = cand.Type().String()
	switch cand.Type() {
	case ice.CandidateTypeHost, ice.CandidateTypePeerReflexive:
	default:
		return false, typ
	}
	addr, _, _ := strings.Cut(cand.Address(), "%") // an IPv6 zone, fe80::1%eth0
	return IsLANAddress(net.ParseIP(addr)), typ
}

// LANCandidateFor is LANCandidateAllowed for a camera whose own LAN address
// may be known (lanIP: the integration's lan_ip for it, "" when unknown). A
// UDP candidate LANCandidateAllowed refuses is kept in a rewritten form when
// it can be tied to that address:
//
//   - a host candidate under an mDNS name (which cannot be resolved from inside
//     a container), or on a public IPv4 address when lan_ip is IPv4 too,
//     becomes a host candidate on the camera's address with the same port. A
//     host candidate's port is the camera's own socket; an SCD923 on protocol
//     3.3 put its public address in its host candidate (02-11-2026: host,
//     srflx and relay all public, none on its LAN address), so this is the
//     only way its webrtc session stays on the LAN;
//   - a server-reflexive candidate whose base (raddr/rport) is the camera's
//     address becomes a host candidate on that base: the socket the camera
//     sends from on the LAN.
//
// A camera that put no LAN host candidate in its answer (the SCD923 on
// protocol 3.3, 02-11-2026: kept 0) can so still stream over the LAN. out
// is the line to use, with the input's "candidate:" prefix kept; class says
// what kind of address the input carried, for the log, never the address.
func LANCandidateFor(line, lanIP string) (out string, allowed bool, typ, class string) {
	if ok, t := LANCandidateAllowed(line); ok {
		return line, true, t, "lan"
	}
	raw := strings.TrimSpace(line)
	raw = strings.TrimPrefix(raw, "a=")
	cand, err := ice.UnmarshalCandidate(raw)
	if err != nil {
		return line, false, "", "unparsable"
	}
	typ = cand.Type().String()
	class = AddressClass(cand.Address())
	own := lanHost(lanIP)
	if own == "" || cand.NetworkType().IsTCP() {
		return line, false, typ, class
	}
	var addr string
	var port int
	switch cand.Type() {
	case ice.CandidateTypeHost:
		// A public IPv4 socket's port says nothing about an IPv6 one.
		ipv4 := net.ParseIP(own).To4() != nil
		if class != "mdns" && (class != "public-ipv4" || !ipv4) {
			return line, false, typ, class
		}
		addr, port = own, cand.Port()
	case ice.CandidateTypeServerReflexive:
		related := cand.RelatedAddress()
		if related == nil || related.Port == 0 || lanHost(related.Address) != own {
			return line, false, typ, class
		}
		addr, port = own, related.Port
	default:
		return line, false, typ, class
	}
	host, err := ice.NewCandidateHost(&ice.CandidateHostConfig{
		Network:    cand.NetworkType().NetworkShort(),
		Address:    addr,
		Port:       port,
		Component:  cand.Component(),
		Priority:   cand.Priority(),
		Foundation: cand.Foundation(),
	})
	if err != nil {
		return line, false, typ, class
	}
	out = host.Marshal()
	if strings.HasPrefix(raw, "candidate:") {
		out = "candidate:" + out
	}
	if strings.HasPrefix(strings.TrimSpace(line), "a=") {
		out = "a=" + out
	}
	return out, true, typ, class
}

// lanHost is addr in its canonical form when it is a literal LAN address,
// else "".
func lanHost(addr string) string {
	if ip := net.ParseIP(addr); ip != nil && IsLANAddress(ip) {
		return ip.String()
	}
	return ""
}

// LANIPProblem says why lanIP, a camera's configured LAN address, cannot be
// used by LANCandidateFor ("" when it can): it is not an IP address, an IPv6
// address with a zone, or not a LAN address. The reason never includes the
// address.
func LANIPProblem(lanIP string) string {
	if lanHost(lanIP) != "" {
		return ""
	}
	if addr, _, zoned := strings.Cut(lanIP, "%"); zoned && net.ParseIP(addr) != nil {
		return "an IPv6 address with a zone"
	}
	if net.ParseIP(lanIP) == nil {
		return "not an IP address"
	}
	return "not a LAN address (RFC 1918, link-local or ULA)"
}

// AddressClass names the kind of address a candidate carries, for logs that
// must not show the address itself.
func AddressClass(addr string) string {
	addr, _, _ = strings.Cut(addr, "%")
	if strings.HasSuffix(strings.ToLower(addr), ".local") {
		return "mdns"
	}
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return "name"
	case ip.IsLoopback():
		return "loopback"
	case IsLANAddress(ip) && ip.To4() != nil:
		return "lan-ipv4"
	case IsLANAddress(ip):
		return "lan-ipv6"
	case ip.To4() != nil:
		return "public-ipv4"
	default:
		return "public-ipv6"
	}
}

// FilterLANCandidates removes every a=candidate line from an SDP that
// LANCandidateFor refuses, rewrites the ones it rewrites (rewritten counts
// those), and leaves all other lines exactly as they were.
func FilterLANCandidates(sdp, lanIP string) (filtered string, kept, dropped, rewritten int) {
	lines := strings.SplitAfter(sdp, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "a=candidate:") {
			body := strings.TrimRight(line, "\r\n")
			ending := line[len(body):]
			newLine, ok, _, _ := LANCandidateFor(body, lanIP)
			if !ok {
				dropped++
				continue
			}
			kept++
			if newLine != body {
				rewritten++
			}
			line = newLine + ending
		}
		out = append(out, line)
	}
	return strings.Join(out, ""), kept, dropped, rewritten
}
