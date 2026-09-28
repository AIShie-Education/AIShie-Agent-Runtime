// Package netguard keeps the calls a hosted agent's model makes to the
// public internet (the API contract's §8, D9). An owner never gives a URL:
// the registry builds each hosted model's endpoint from an official
// provider's host (registry.Offers, registry.Official). What that host's
// name resolves to is not the owner's to choose either, and still may be
// anything: a DNS answer, or a redirect, could point it at the runtime's
// own machine, its network, or a cloud's metadata service. The guard
// refuses, at the moment of each connection, every address that is not a
// public one, whatever name led to it, and the hosted-model client follows
// no redirect at all.
//
// With a proxy, the connection is to the proxy, the operator's own, which
// is let through: the proxy resolves the provider's name and must refuse
// these addresses itself (docs/deploying.md says so).
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// ErrBlocked is a connection refused: its address is not a public one.
var ErrBlocked = errors.New("netguard: the address is not a public one; the runtime does not call it for a hosted agent")

// ErrRedirect is a redirect refused: a hosted agent's model is called at
// its official endpoint, and nowhere it sends the runtime.
var ErrRedirect = errors.New("netguard: a redirect is not followed for a hosted agent's model")

// blocked are the ranges no hosted model is called in: this host and its
// networks (loopback, private, unique local), link-local (the cloud
// metadata services' 169.254.169.254 among them), carrier-grade NAT, and
// what is no one's to call (this network, IETF protocol assignments,
// benchmarking, documentation, reserved, multicast, broadcast, the
// unspecified addresses).
var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// Embedded IPv4 addresses: NAT64 (64:ff9b::/96, and the local-use
// 64:ff9b:1::/48) and 6to4 (2002::/16) reach the IPv4 address they hold,
// which must be a public one too.
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

// Public reports whether ip is an address a hosted agent's model may be
// called at: none of the blocked ranges, in either family, an IPv4 address
// written in IPv6 (::ffff:10.0.0.1) or held in one (NAT64, 6to4) judged as
// the IPv4 address it is.
func Public(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		b := ip.As16()
		switch {
		case nat64.Contains(ip), nat64Local.Contains(ip):
			if !Public(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})) {
				return false
			}
		case sixToFour.Contains(ip):
			if !Public(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})) {
				return false
			}
		}
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() && !ip.IsUnspecified() && !ip.IsInterfaceLocalMulticast()
}

// Resolver resolves a host's name to its addresses: net.DefaultResolver,
// or a test's.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer makes the hosted-model client's connections: it resolves a
// name itself and dials only public addresses, and checks the address
// again as the socket connects (Control), so that no address that is not
// public is ever connected to, whatever the name resolved to. Addresses
// Proxy names are dialled as they are, unguarded: the operator's proxy.
type Dialer struct {
	// Resolver resolves names; net.DefaultResolver when nil.
	Resolver Resolver
	// Timeout bounds a connection; 30 s when zero.
	Timeout time.Duration
	// Dial connects, once the address is judged; a net.Dialer with Control
	// when nil. For tests.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	mu      sync.Mutex
	proxies map[string]bool
}

// AllowProxy lets connections to a proxy's address (host:port) through:
// Transport calls it for each proxy its Proxy function names.
func (d *Dialer) AllowProxy(address string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.proxies == nil {
		d.proxies = map[string]bool{}
	}
	d.proxies[address] = true
}

func (d *Dialer) proxy(address string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.proxies[address]
}

// DialContext connects to address (host:port), a public address of the
// host's; a host with none is refused, ErrBlocked.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.proxy(address) {
		return d.dial(ctx, network, address, false)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		r := d.Resolver
		if r == nil {
			r = net.DefaultResolver
		}
		if addrs, err = r.LookupNetIP(ctx, "ip", host); err != nil {
			return nil, err
		}
	}
	var public []netip.Addr
	for _, a := range addrs {
		if Public(a) {
			public = append(public, a)
		}
	}
	if len(public) == 0 {
		return nil, fmt.Errorf("dial %s: %w", host, ErrBlocked)
	}
	var last error
	for _, a := range public {
		conn, err := d.dial(ctx, network, net.JoinHostPort(a.String(), port), true)
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

func (d *Dialer) dial(ctx context.Context, network, address string, guarded bool) (net.Conn, error) {
	if d.Dial != nil {
		return d.Dial(ctx, network, address)
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	nd := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if guarded {
		nd.Control = control
	}
	return nd.DialContext(ctx, network, address)
}

// control checks the address a socket is about to connect to.
func control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !Public(ip) {
		return fmt.Errorf("connect %s: %w", host, ErrBlocked)
	}
	return nil
}

// Transport is base's copy that dials through d: every connection it makes
// is to a public address, or to a proxy base's Proxy names. HTTP/2 is kept
// as base has it.
func Transport(base *http.Transport, d *Dialer) *http.Transport {
	tr := base.Clone()
	tr.DialContext = d.DialContext
	tr.DialTLSContext = nil
	if proxy := base.Proxy; proxy != nil {
		tr.Proxy = func(r *http.Request) (*url.URL, error) {
			u, err := proxy(r)
			if err == nil && u != nil {
				d.AllowProxy(proxyAddress(u))
			}
			return u, err
		}
	}
	return tr
}

// proxyAddress is the host:port a proxy's URL is dialled at.
func proxyAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// NoRedirects is c's copy that follows no redirect: a 3xx is refused,
// ErrRedirect, and nothing is sent where it points.
func NoRedirects(c *http.Client) *http.Client {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrRedirect }
	return &cp
}

// Client is the hosted-model client made from the egress client: its
// transport's copy dialling through a Dialer, and no redirect followed.
// The egress client's transport must be an *http.Transport.
func Client(egress *http.Client) (*http.Client, error) {
	base := http.DefaultTransport
	if egress != nil && egress.Transport != nil {
		base = egress.Transport
	}
	tr, ok := base.(*http.Transport)
	if !ok {
		return nil, errors.New("netguard: the egress client's transport is not an *http.Transport")
	}
	c := &http.Client{}
	if egress != nil {
		*c = *egress
	}
	c.Transport = Transport(tr, &Dialer{})
	return NoRedirects(c), nil
}
