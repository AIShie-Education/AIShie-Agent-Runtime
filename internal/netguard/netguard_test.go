package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
)

// Public refuses this host, its networks, link-local and the metadata
// service, carrier-grade NAT and the ranges no one calls, in both families
// and in IPv4 written or held in IPv6, the IPv4-compatible and
// IPv4-translated forms whole; it takes public addresses.
func TestPublic(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.8.9.10", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254", "169.254.0.1",
		"100.64.0.1", "100.127.255.254", "0.0.0.0", "0.1.2.3", "192.0.0.8", "198.18.0.1", "198.19.255.255", "240.0.0.1",
		"255.255.255.255", "224.0.0.1", "239.1.2.3", "192.0.2.10",
		"::1", "::", "fd00::1", "fc00::1", "fe80::1", "fe80::1%eth0", "ff02::1", "2001:db8::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::7f00:1", "2002:a00:1::1", "2002:7f00:1::1",
		"::7f00:1", "::a9fe:a9fe", "::a00:1", "::808:808", "::ffff:0:7f00:1", "::ffff:0:a9fe:a9fe", "::ffff:0:808:808",
	} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("%s is taken as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "104.18.6.192", "172.32.0.1", "100.128.0.1", "2606:4700::6810:84e5", "::ffff:8.8.8.8",
		"64:ff9b::808:808", "2002:808:808::1"} {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s is refused", s)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("no address is public")
	}
}

// resolver answers every name with the same addresses.
type resolver []string

func (r resolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range r {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

// A name that resolves only to addresses not public is refused before any
// connection; one that resolves to some public addresses connects only to
// those; a literal address is judged as it is; a proxy's address is let
// through as it is.
func TestDialer(t *testing.T) {
	var mu sync.Mutex
	var dialled []string
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dialled = append(dialled, address)
		return nil, errors.New("no network in this test")
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fd00::1", "::ffff:10.0.0.1", "::7f00:1", "::ffff:0:a9fe:a9fe"} {
		d := &Dialer{Resolver: resolver{ip}, Dial: dial}
		if _, err := d.DialContext(t.Context(), "tcp", "api.openai.com:443"); !errors.Is(err, ErrBlocked) {
			t.Errorf("a name that resolves to %s: %v", ip, err)
		}
		if _, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort(ip, "443")); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s itself: %v", ip, err)
		}
	}
	if len(dialled) != 0 {
		t.Fatalf("dialled %v", dialled)
	}
	d := &Dialer{Resolver: resolver{"10.0.0.1", "8.8.8.8", "::1", "2606:4700::1"}, Dial: dial}
	_, _ = d.DialContext(t.Context(), "tcp", "api.openai.com:443")
	if len(dialled) != 2 || dialled[0] != "8.8.8.8:443" || dialled[1] != "[2606:4700::1]:443" {
		t.Errorf("dialled %v, want the public addresses alone", dialled)
	}
	d.AllowProxy("10.9.8.7:3128")
	_, _ = d.DialContext(t.Context(), "tcp", "10.9.8.7:3128")
	if dialled[len(dialled)-1] != "10.9.8.7:3128" {
		t.Errorf("the proxy was not dialled: %v", dialled)
	}
	// The socket's own check refuses an address not public however it
	// was reached.
	if err := control("tcp4", "127.0.0.1:443", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("control: %v", err)
	}
	if err := control("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Errorf("control of a public address: %v", err)
	}
}

// The hosted-model client connects to no address that is not public, and
// follows no redirect; through a proxy, the proxy is called.
func TestClient(t *testing.T) {
	var hits sync.Map
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Store("target", true)
		_, _ = io.WriteString(w, "you should not be here")
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/latest/meta-data", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	c, err := Client(&http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(target.URL)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("a server on loopback: %v", err)
	}
	// No redirect is followed, even through a transport that may connect
	// anywhere.
	open := NoRedirects(&http.Client{})
	resp, err = open.Get(redirector.URL)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrRedirect) {
		t.Errorf("a redirect: %v", err)
	}
	if _, hit := hits.Load("target"); hit {
		t.Error("the redirect was followed")
	}

	// Through a proxy (the operator's, here on loopback), the proxy is
	// called: it is what judges the provider's name.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Store("proxy", r.URL.String())
		_, _ = io.WriteString(w, "proxied")
	}))
	t.Cleanup(proxy.Close)
	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(pu)
	pc, err := Client(&http.Client{Transport: base})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = pc.Get("http://api.provider.example/v1/models")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	_ = resp.Body.Close()
	if got, _ := hits.Load("proxy"); got != "http://api.provider.example/v1/models" {
		t.Errorf("the proxy saw %v", got)
	}
	if _, err := Client(&http.Client{Transport: roundTripper(nil)}); err == nil {
		t.Error("a transport that is not an *http.Transport was taken")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
