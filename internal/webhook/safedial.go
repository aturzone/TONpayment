package webhook

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// blockPrivate is a net.Dialer Control hook that refuses to connect to a non-public
// IP. It runs AFTER DNS resolution, with the concrete IP about to be dialed, so it
// defeats DNS-rebinding: a merchant webhook host that resolves to 127.0.0.1, 10.x,
// 169.254.169.254 (cloud metadata), ::1, etc. is rejected at connect time. That
// closes the SSRF vector a signed settlement POST would otherwise open on a shared
// host — the authoritative guard (ValidateURL is just an early, friendlier reject).
func blockPrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("webhook: unresolved dial address %q", host)
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("webhook: refusing to connect to non-public address %s", ip)
	}
	return nil
}

// isPublicIP reports whether ip is a routable public address (not loopback, RFC1918
// / ULA private, link-local, multicast, or unspecified).
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

// redirectSafeClient refuses redirects but — unlike SecureClient — does NOT block
// private/loopback targets. It backs the TRUSTED single-tenant global webhook URL
// (set by the operator via env), where pointing at internal infrastructure is a
// legitimate choice. The SSRF dial guard is reserved for untrusted, merchant-
// supplied URLs (SecureClient, used by the multi-tenant router).
func redirectSafeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("webhook: redirects are not followed")
		},
	}
}

// SecureClient builds the HTTP client used for UNTRUSTED (merchant-supplied) webhook
// delivery: it refuses redirects (a redirect could forward the signed invoice to an
// internal host) and refuses to dial non-public IPs (the SSRF/rebinding guard above).
func SecureClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   blockPrivate,
	}).DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("webhook: redirects are not followed")
		},
	}
}

// ValidateURL rejects obviously unsafe webhook targets at creation time so the
// merchant gets a clear error instead of silent delivery failures: a non-http(s)
// scheme, a missing host, localhost, or a literal private/loopback/link-local IP.
// A DNS name that *resolves* into private space is caught later by blockPrivate at
// delivery — that, not this, is the authoritative protection. requireHTTPS forces
// TLS (production).
func ValidateURL(raw string, requireHTTPS bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("invalid url")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if requireHTTPS {
			return errors.New("url must use https")
		}
	default:
		return errors.New("url must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url must have a host")
	}
	if strings.EqualFold(host, "localhost") {
		return errors.New("url host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return errors.New("url host is not allowed")
	}
	return nil
}
