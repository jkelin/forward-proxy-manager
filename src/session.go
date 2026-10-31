package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/cookiejar"
	"golang.org/x/net/publicsuffix"
)

// warmupUrlHeader is the client control header naming the page to visit before the real request.
const warmupUrlHeader = "x-warmup-url"

// warmupStatusHeader tells the client what happened to the session: fresh, reused or failed.
const warmupStatusHeader = "X-Fpm-Warmup"

// parseWarmupUrl validates the x-warmup-url header value (and the gRPC warmup_url field).
// Edge cases: an empty value means no warm-up (nil, nil); everything parseAbsoluteHttpUrl rejects fails.
func parseWarmupUrl(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}

	parsed, err := parseAbsoluteHttpUrl(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", warmupUrlHeader, err)
	}

	return parsed, nil
}

// parseAbsoluteHttpUrl accepts only absolute http(s) URLs with a host.
// Edge cases: relative, non-http(s), host-less (e.g. "https://:443/") URLs and ports outside 1-65535 fail.
func parseAbsoluteHttpUrl(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}

	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return nil, fmt.Errorf("%q must be an absolute http(s) URL", raw)
	}

	if port := parsed.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("%q has an invalid port %q", raw, port)
		}
	}

	return parsed, nil
}

// isSessionRejected reports whether a response means the proxy's session for a warm-up host is burned.
// Only requests that opted into warm-up manage a session, so other 403s keep the jar.
func isSessionRejected(code int, hasWarmup bool) bool {
	return hasWarmup && code == http.StatusForbidden
}

// registrableDomain returns the eTLD+1 used for same-site checks.
// Edge cases: DNS names compare case-insensitively; IP literals and hosts without a registrable domain
// (e.g. "localhost", "co.uk") compare as the bare host.
func registrableDomain(host string) string {
	host = strings.ToLower(host)
	if net.ParseIP(host) != nil {
		return host
	}

	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}

	return domain
}

// isSameOrigin compares scheme, host and effective port; DNS host names are case-insensitive.
func isSameOrigin(first *url.URL, second *url.URL) bool {
	return first.Scheme == second.Scheme && strings.EqualFold(withoutDefaultPort(first), withoutDefaultPort(second))
}

// fetchSite computes Sec-Fetch-Site for a navigation from initiator to target (schemeful same-site, as in Chrome/Firefox).
func fetchSite(initiator *url.URL, target *url.URL) string {
	if isSameOrigin(initiator, target) {
		return "same-origin"
	}

	if initiator.Scheme == target.Scheme && registrableDomain(initiator.Hostname()) == registrableDomain(target.Hostname()) {
		return "same-site"
	}

	return "cross-site"
}

// refererFor applies the browsers' default strict-origin-when-cross-origin policy.
// Edge cases: same-origin sends the full URL without fragment or credentials; cross-origin sends only the origin;
// an https -> http downgrade sends nothing ("").
func refererFor(initiator *url.URL, target *url.URL) string {
	if initiator.Scheme == "https" && target.Scheme == "http" {
		return ""
	}

	if isSameOrigin(initiator, target) {
		stripped := *initiator
		stripped.User = nil
		stripped.Fragment = ""
		stripped.RawFragment = ""

		return stripped.String()
	}

	return initiator.Scheme + "://" + withoutDefaultPort(initiator) + "/"
}

// navigationHeaders turns identity headers (a typed-URL navigation) into a link click from initiator to target.
// The identity's header order already lists "referer", so only the values change.
// Edge cases: a referer already present in identityHeaders is removed when the policy sends none.
func navigationHeaders(identityHeaders http.Header, initiator *url.URL, target *url.URL) http.Header {
	headers := identityHeaders.Clone()
	headers["sec-fetch-site"] = []string{fetchSite(initiator, target)}

	if referer := refererFor(initiator, target); referer != "" {
		headers["referer"] = []string{referer}
	} else {
		delete(headers, "referer")
	}

	return headers
}

// sessionJar is one ProxyClient's cookie jar (RFC 6265 domain/path/expiry rules via fhttp's cookiejar).
// Reset swaps in an empty jar atomically, so concurrent requests never observe a half-cleared jar.
type sessionJar struct {
	jar atomic.Pointer[cookiejar.Jar]
}

func newEmptyCookieJar() *cookiejar.Jar {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		// cookiejar.New never fails with a non-nil options struct; a failure means a library contract change.
		panic(err)
	}

	return jar
}

func newSessionJar() *sessionJar {
	session := &sessionJar{}
	session.jar.Store(newEmptyCookieJar())

	return session
}

func (session *sessionJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	session.jar.Load().SetCookies(target, cookies)
}

func (session *sessionJar) Cookies(target *url.URL) []*http.Cookie {
	return session.jar.Load().Cookies(target)
}

func (session *sessionJar) Reset() {
	session.jar.Store(newEmptyCookieJar())
}

// resetSession drops the client's cookies and warm-up marks after the target rejected the session.
func (client *ProxyClient) resetSession() {
	client.warmupLock.Lock()
	defer client.warmupLock.Unlock()

	client.cookies.Reset()
	clear(client.warmedHosts)
}

// ensureWarm visits warmupUrl with the identity's typed-URL headers unless target's host is already warmed.
// Returns "fresh" or "reused"; a non-2xx warm-up returns "failed" with that response so the caller can hand it back.
// Edge cases: a jar that only holds cookies from rejections (e.g. Akamai sets them on 403) does not count as warm,
// hence the explicit warmedHosts mark; expired cookies (empty jar for target) force a new warm-up, so a warm-up
// page that sets no cookies is repeated before every request.
// Concurrent requests on one client wait for a single warm-up instead of each sending one.
func (client *ProxyClient) ensureWarm(ctx context.Context, warmupUrl *url.URL, target *url.URL) (string, *Response, error) {
	client.warmupLock.Lock()
	defer client.warmupLock.Unlock()

	host := target.Hostname()
	if _, warmed := client.warmedHosts[host]; warmed && len(client.cookies.Cookies(target)) > 0 {
		return "reused", nil, nil
	}

	start := time.Now()
	response, err := send(ctx, client.httpClient, "GET", warmupUrl, client.headers)
	if err != nil {
		return "", nil, err
	}

	log.Printf("%s warmup %s %d, %dms", client.id, warmupUrl.String(), response.Code, time.Since(start).Milliseconds())

	if response.Code < 200 || response.Code > 299 {
		return "failed", response, nil
	}

	client.warmedHosts[host] = struct{}{}

	return "fresh", nil, nil
}
