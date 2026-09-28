package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// The middleware, outermost first (the API contract, §3.4): a log line and
// metrics for every request (logged); a panic answered as 500 (recovered);
// the headers of every answer (secured); the client's address (addressed);
// cross-origin writes refused (http.CrossOriginProtection); routing, with
// no_route and method_not_allowed (routed). Each route then takes the
// per-address or per-person limits, and its assertion (public, authed),
// then its query and body (noBody), then does what it does.

// recorder keeps what the log line and the metrics say of a request: its
// status, route, reason, person and hosted agent.
type recorder struct {
	http.ResponseWriter
	status int
	route  string
	reason string
	actor  string
	agent  string
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logged writes one line for every request, at info, and counts it: its
// method, route, status, reason, person and how long it took; never its
// Authorization, its body, a token, a key, a name or an email.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		route := rec.route
		if route == "" {
			route = "none"
		}
		code := "ok"
		if rec.status >= 400 {
			code = rec.reason
			if code == "" {
				code = strconv.Itoa(rec.status)
			}
		}
		took := time.Since(start)
		s.m.requests.WithLabelValues(route, code).Inc()
		s.m.seconds.WithLabelValues(route).Observe(took.Seconds())
		s.o.Log.LogAttrs(r.Context(), slog.LevelInfo, "api request", slog.String("method", r.Method), slog.String("route", rec.route),
			slog.Int("status", rec.status), slog.String("reason", rec.reason), slog.String("actor", rec.actor),
			slog.String("agent", rec.agent), slog.Int64("ms", took.Milliseconds()))
	})
}

// recovered answers 500 internal for a handler that panicked, logging where
// and nothing of the request but its method and route.
func (s *Server) recovered(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && err == http.ErrAbortHandler { //nolint:errorlint // panicked with as it is
				panic(v)
			}
			route := ""
			if rec, ok := w.(*recorder); ok {
				route = rec.route
			}
			s.o.Log.Error("the API panicked", "method", r.Method, "route", route)
			WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "something went wrong on the runtime's side"})
		}()
		next.ServeHTTP(w, r)
	})
}

// secured sets what every answer carries (the API contract, §1): it is
// never cached (GET /info sets its own), it is what it says it is, it runs
// and embeds nothing, and it sends no referrer. There is no CORS: the API
// is same-origin.
func secured(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type addrKey struct{}

// addressed works out the client's address, for the audit and the
// per-address limits: the peer's, or, when the peer is a trusted proxy,
// the last address in X-Forwarded-For that is not one.
func (s *Server) addressed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), addrKey{}, s.clientIP(r))))
	})
}

// clientAddr is the request's client address, as addressed found it.
func clientAddr(r *http.Request) string {
	a, _ := r.Context().Value(addrKey{}).(string)
	return a
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.trusted(net.ParseIP(host)) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break
		}
		if !s.trusted(ip) {
			return ip.String()
		}
	}
	return host
}

func (s *Server) trusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range s.o.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// routed serves the mux's routes, and answers the rest: 405 with Allow
// for a route asked with a method it does not take (OPTIONS among them),
// and 404 no_route for anything else, a path the mux would tidy by
// redirecting included (the API redirects nowhere). /healthz, /metrics and
// /status are among the rest: they are HTTP_ADDR's alone.
func (s *Server) routed(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noRoute := Error{Code: CodeNotFound, Reason: ReasonNoRoute, Message: "no such route under " + Prefix}
		p := r.URL.Path
		if !strings.HasPrefix(p, Prefix) || p != path.Clean(p) || r.URL.RawPath != "" {
			WriteError(w, noRoute)
			return
		}
		h, pattern := mux.Handler(r)
		if pattern != "" {
			if rec, ok := w.(*recorder); ok {
				rec.route = pattern
			}
			mux.ServeHTTP(w, r)
			return
		}
		probe := &probeWriter{header: http.Header{}}
		h.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			WriteError(w, Error{Code: CodeMethodNotAllowed, Reason: ReasonMethodNotAllowed,
				Message: r.Method + " is not something this route takes; it takes " + probe.header.Get("Allow")})
			return
		}
		WriteError(w, noRoute)
	})
}

// probeWriter takes a response and keeps only its status and headers.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

// public serves h to anyone within their address's allowance, a request
// taking no query and no body.
func (s *Server) public(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := s.perIP.take(clientAddr(r), s.o.Now()); !ok {
			writeRateLimited(w, wait)
			return
		}
		if !noBody(w, r) {
			return
		}
		h(w, r)
	})
}

// writeRateLimited answers 429 rate_limited, saying when to try again.
func writeRateLimited(w http.ResponseWriter, wait int) {
	WriteError(w, Error{Code: CodeRateLimited, Reason: ReasonRateLimited, Message: "too many requests: wait, then try again",
		Details: map[string]any{"retry_after_seconds": max(wait, 1)}})
}

// ParseProxies reads API_TRUSTED_PROXIES' entries: CIDRs, or single
// addresses.
func ParseProxies(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for i, e := range entries {
		if _, n, err := net.ParseCIDR(e); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(e)
		if ip == nil {
			return nil, fmt.Errorf("API_TRUSTED_PROXIES: entry %d is neither a CIDR nor an address", i+1)
		}
		bits := 8 * net.IPv6len
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 8*net.IPv4len
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}
