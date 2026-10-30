package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/inhies/go-bytesize"
	"github.com/throttled/throttled/v2"
	"github.com/throttled/throttled/v2/store/memstore"
	"golang.org/x/sync/semaphore"
)

type ProxyConfig struct {
	host     string
	port     int64
	username string
	password string
}

// socksHostFailures are SOCKS5 CONNECT replies (as x/net/proxy words them) blaming the target, not the proxy.
var socksHostFailures = []string{
	"unknown error network unreachable",
	"unknown error host unreachable",
	"unknown error connection refused",
	"unknown error TTL expired",
}

// classifyTransportError maps a failed attempt to a response status; false means the error is not recognised.
// Edge cases: timeouts win over SOCKS errors (a proxy too slow to answer counts as a timeout); a SOCKS CONNECT
// reply blaming the target is HostUnreachable, while failing to reach, authenticate with or get a usable reply
// from the proxy is ProxyUnreachable.
func classifyTransportError(err error) (ResponseStatus, bool) {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return ResponseStatusTimeout, true
	}

	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "socks connect" {
		return 0, false
	}

	if opErr.Err != nil && slices.Contains(socksHostFailures, opErr.Err.Error()) {
		return ResponseStatusHostUnreachable, true
	}

	return ResponseStatusProxyUnreachable, true
}

// handleError turns a transport error into a status response, or returns the error when nothing matches.
// Edge cases: a done caller context wins over every other classification and is returned as its ctx error.
func (client *ProxyClient) handleError(callerCtx context.Context, method string, uri *url.URL, err error) (*Response, error) {
	if callerCtx.Err() != nil {
		return nil, callerCtx.Err()
	}

	status, known := classifyTransportError(err)
	if !known {
		log.Printf("%s %s %s %v", client.id, method, uri.String(), err)
		return nil, err
	}

	if status != ResponseStatusTimeout {
		log.Printf("%s %s %s %v", client.id, method, uri.String(), err)
	}

	return &Response{Status: status}, nil
}

// makeRequestWithClient sends req through this exit, warming the session first when req asks for it.
// The target scheme is req.Url's own; req.Url itself is never modified.
func (client *ProxyClient) makeRequestWithClient(req *ActiveRequest, timeout time.Duration) (*Response, error) {
	start := time.Now()

	requestCtx, cancelFn := context.WithTimeout(req.Context, timeout)
	defer cancelFn()

	target := *req.Url
	target.Host = withoutDefaultPort(&target)

	headers := client.headers
	warmupStatus := ""

	if req.WarmupUrl != nil {
		status, failedResponse, err := client.ensureWarm(requestCtx, req.WarmupUrl, &target)
		if err != nil {
			return client.handleError(req.Context, req.Method, req.WarmupUrl, err)
		}

		if failedResponse != nil {
			client.resetSession()
			failedResponse.Headers.Set(warmupStatusHeader, status)

			return failedResponse, nil
		}

		warmupStatus = status
		headers = navigationHeaders(headers, req.WarmupUrl, &target)
	}

	response, err := client.send(requestCtx, req.Method, &target, headers)
	if err != nil {
		return client.handleError(req.Context, req.Method, &target, err)
	}

	log.Printf("%dp %s %s %s %d %s, %dms", req.Priority, client.id, req.Method, target.String(), response.Code, bytesize.New(float64(len(response.Body))), time.Since(start).Milliseconds())

	if req.WarmupUrl != nil {
		if isSessionRejected(response.Code, true) {
			client.resetSession()
		}

		response.Headers.Set(warmupStatusHeader, warmupStatus)
	}

	return response, nil
}

// send performs one request with the given headers; cookies come from and go to the client's jar.
// Only Content-Type and Content-Disposition are passed back to callers.
func (client *ProxyClient) send(ctx context.Context, method string, target *url.URL, headers fhttp.Header) (*Response, error) {
	request, err := fhttp.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, err
	}

	request.Header = headers.Clone()

	// tls-client negotiates h2/http1.1 via the profile's ALPN, like the impersonated browser.
	resp, err := client.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("Error closing body in send: %v", err)
		}
	}()

	// Fake headers set Accept-Encoding explicitly, so the transport leaves the body encoded unless it reports otherwise.
	contentEncoding := resp.Header.Get("Content-Encoding")
	if resp.Uncompressed {
		contentEncoding = ""
	}

	body, err := decodeResponseBody(resp.Body, contentEncoding)
	if err != nil {
		return nil, err
	}

	response := Response{
		Status:  ResponseStatusOk,
		Code:    resp.StatusCode,
		Body:    body,
		Headers: http.Header{},
	}

	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		response.Headers.Set("Content-Type", contentType)
	}

	if contentDisposition := resp.Header.Get("Content-Disposition"); contentDisposition != "" {
		response.Headers.Set("Content-Disposition", contentDisposition)
	}

	return &response, nil
}

// withoutDefaultPort drops the scheme's default port, as browsers omit it from Host/:authority.
// MITM CONNECT targets arrive as "host:443", which would otherwise leak as ":authority: host:443".
// Edge cases: non-default ports are kept; IPv6 literals keep their brackets.
func withoutDefaultPort(target *url.URL) string {
	port := target.Port()
	isDefaultPort := (target.Scheme == "https" && port == "443") || (target.Scheme == "http" && port == "80")
	if !isDefaultPort {
		return target.Host
	}

	hostname := target.Hostname()
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]"
	}

	return hostname
}

// proxyLine is one "ip:port:username:password" proxy list entry; the password may contain ':'.
var proxyLine = regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5}):([^:]+):(.+)$`)

// parseProxyList reads one proxy per line in "ip:port:username:password" form.
// Edge cases: LF and CRLF line endings both work, as does a last line without a newline; blank lines are ignored;
// lines with an invalid IPv4 address, a port outside 1-65535 or missing credentials are skipped with a log line.
func parseProxyList(body string) []ProxyConfig {
	configs := make([]ProxyConfig, 0)

	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		match := proxyLine.FindStringSubmatch(line)
		if match == nil {
			log.Printf("Skipping malformed proxy list line %q", line)
			continue
		}

		address, err := netip.ParseAddr(match[1])
		port, portErr := strconv.ParseInt(match[2], 10, 32)
		if err != nil || !address.Is4() || portErr != nil || port < 1 || port > 65535 {
			log.Printf("Skipping invalid proxy address %s:%s", match[1], match[2])
			continue
		}

		configs = append(configs, ProxyConfig{
			host:     match[1],
			port:     port,
			username: match[3],
			password: match[4],
		})
	}

	return configs
}

// fetchProxyList downloads and parses the proxy list.
// Edge cases: non-2xx answers fail instead of yielding an empty list.
func fetchProxyList(ctx context.Context, listUrl string) ([]ProxyConfig, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, listUrl, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("Error closing body in fetchProxyList: %v", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("proxy list %s answered %d", listUrl, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return parseProxyList(string(body)), nil
}

// exitLimits bounds how hard one exit is used.
type exitLimits struct {
	requestsPerMin int
	burst          int
	// unreachableRetry is how long an exit whose proxy failed stays out of scheduling.
	unreachableRetry time.Duration
	// rateLimitedBackoff is how long an exit avoids a host that answered 429.
	rateLimitedBackoff time.Duration
	clock              func() time.Time
}

// ProxyClient is one SOCKS exit with one stable browser identity: TLS/HTTP2 profile, matching headers
// and its own cookie session, like one real browser on one IP.
type ProxyClient struct {
	id          string
	httpClient  tls_client.HttpClient
	headers     fhttp.Header
	cookies     *sessionJar
	warmupLock  sync.Mutex
	warmedHosts map[string]struct{}
	limiter     *throttled.GCRARateLimiterCtx
	limits      exitLimits
	// availabilityLock guards unreachableUntil and backoffUntil.
	availabilityLock sync.Mutex
	unreachableUntil time.Time
	backoffUntil     map[string]time.Time
}

// newLimiter builds a per-host GCRA limiter reading time from clock.
// Edge cases: requestsPerMin must be positive and burst non-negative.
func newLimiter(requestsPerMin int, burst int, clock func() time.Time) (*throttled.GCRARateLimiterCtx, error) {
	if requestsPerMin <= 0 || burst < 0 {
		return nil, fmt.Errorf("invalid rate limit: %d requests/min, burst %d", requestsPerMin, burst)
	}

	store, err := memstore.New(65536)
	if err != nil {
		return nil, err
	}

	store.SetTimeNow(clock)

	quota := throttled.RateQuota{
		MaxRate:  throttled.PerMin(requestsPerMin),
		MaxBurst: burst,
	}

	return throttled.NewGCRARateLimiterCtx(throttled.WrapStoreWithContext(store), quota)
}

// newProxyClient builds a client impersonating profile; options add the transport (proxy, TLS verification).
func newProxyClient(id string, profile profiles.ClientProfile, limits exitLimits, options ...tls_client.HttpClientOption) (*ProxyClient, error) {
	limiter, err := newLimiter(limits.requestsPerMin, limits.burst, limits.clock)
	if err != nil {
		return nil, err
	}

	cookies := newSessionJar()
	idleConnTimeout := 60 * time.Second

	options = append(options,
		tls_client.WithClientProfile(profile),
		tls_client.WithTimeoutSeconds(10),
		tls_client.WithCookieJar(cookies),
		tls_client.WithTransportOptions(&tls_client.TransportOptions{
			MaxIdleConns:    1024,
			MaxConnsPerHost: 12,
			IdleConnTimeout: &idleConnTimeout,
		}),
	)
	if shufflesTlsExtensions(profile) {
		options = append(options, tls_client.WithRandomTLSExtensionOrder())
	}

	httpClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}

	return &ProxyClient{
		id:           id,
		httpClient:   httpClient,
		headers:      getFakeHeaders(profile),
		cookies:      cookies,
		warmedHosts:  make(map[string]struct{}),
		limiter:      limiter,
		limits:       limits,
		backoffUntil: make(map[string]time.Time),
	}, nil
}

func (client *ProxyClient) markUnreachable() {
	log.Printf("Marking client %s unreachable", client.id)

	client.availabilityLock.Lock()
	defer client.availabilityLock.Unlock()

	client.unreachableUntil = client.limits.clock().Add(client.limits.unreachableRetry)
}

func (client *ProxyClient) backOff(host HostInfo) {
	client.availabilityLock.Lock()
	defer client.availabilityLock.Unlock()

	client.backoffUntil[host.host] = client.limits.clock().Add(client.limits.rateLimitedBackoff)
}

// RateLimit reports whether host is limited on this exit; when it is not, one request token is consumed.
// Edge cases: an unreachable exit and a 429 back-off both limit without consuming tokens, with RetryAfter set to
// the remaining wait; expired back-offs are forgotten.
func (client *ProxyClient) RateLimit(host HostInfo) (bool, throttled.RateLimitResult, error) {
	now := client.limits.clock()

	client.availabilityLock.Lock()
	if wait := client.unreachableUntil.Sub(now); wait > 0 {
		client.availabilityLock.Unlock()
		return true, throttled.RateLimitResult{RetryAfter: wait}, nil
	}

	if until, found := client.backoffUntil[host.host]; found {
		if wait := until.Sub(now); wait > 0 {
			client.availabilityLock.Unlock()
			return true, throttled.RateLimitResult{RetryAfter: wait}, nil
		}

		delete(client.backoffUntil, host.host)
	}
	client.availabilityLock.Unlock()

	return client.limiter.RateLimitCtx(context.Background(), host.host, 1)
}

// getExternalProxyIp asks ipCheckUrl, through client, for the exit's public IP.
// Edge cases: non-OK statuses, non-2xx answers and bodies that are not an IP address fail; whitespace is trimmed.
func getExternalProxyIp(ctx context.Context, client *ProxyClient, ipCheckUrl *url.URL, timeout time.Duration) (string, error) {
	target := *ipCheckUrl
	req := &ActiveRequest{
		Url:      &target,
		Method:   http.MethodGet,
		Priority: 10000,
		Host:     HostInfo{host: target.Hostname()},
		Context:  ctx,
	}

	response, err := client.makeRequestWithClient(req, timeout)
	if err != nil {
		return "", err
	}

	if response.Status != ResponseStatusOk || response.Code < 200 || response.Code > 299 {
		return "", fmt.Errorf("ip check %s failed: status %d, code %d", ipCheckUrl, response.Status, response.Code)
	}

	ip := strings.TrimSpace(string(response.Body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("ip check %s returned %q, not an IP address", ipCheckUrl, ip)
	}

	return ip, nil
}

// proxyManager turns the proxy list into ready exits.
type proxyManager struct {
	listUrl        string
	ipCheckUrl     *url.URL
	ipCheckTimeout time.Duration
	limits         exitLimits
	newProfile     func() profiles.ClientProfile
	// clientOptions are added to every exit's client, e.g. TLS verification settings.
	clientOptions []tls_client.HttpClientOption
	// publish receives the full list of ready exits every time one more becomes ready.
	publish        func([]proxyExit)
	maxConcurrency int64
}

// connect builds the exit for config and names it after its public IP.
func (manager *proxyManager) connect(ctx context.Context, config ProxyConfig) (*ProxyClient, error) {
	profile := manager.newProfile()
	log.Printf("Connecting to proxy %s:%d as %s", config.host, config.port, profile.GetClientHelloStr())

	// socks5h resolves target hostnames on the proxy, matching the previous x/net/proxy dialer.
	proxyUrl := url.URL{
		Scheme: "socks5h",
		User:   url.UserPassword(config.username, config.password),
		Host:   net.JoinHostPort(config.host, strconv.FormatInt(config.port, 10)),
	}

	options := append(slices.Clone(manager.clientOptions), tls_client.WithProxyUrl(proxyUrl.String()))

	client, err := newProxyClient(config.host, profile, manager.limits, options...)
	if err != nil {
		return nil, err
	}

	ip, err := getExternalProxyIp(ctx, client, manager.ipCheckUrl, manager.ipCheckTimeout)
	if err != nil {
		return nil, err
	}

	client.id = ip

	return client, nil
}

// run loads the proxy list and connects the proxies concurrently, publishing the growing list of ready exits.
// Edge cases: proxies that fail to connect are skipped; a failing list download is returned; ctx cancellation stops
// starting new connections. Returns after every started connection attempt finished.
func (manager *proxyManager) run(ctx context.Context) error {
	configs, err := fetchProxyList(ctx, manager.listUrl)
	if err != nil {
		return err
	}

	connections := semaphore.NewWeighted(manager.maxConcurrency)

	var readyLock sync.Mutex
	var attempts sync.WaitGroup
	ready := make([]proxyExit, 0, len(configs))

	for _, config := range configs {
		if err := connections.Acquire(ctx, 1); err != nil {
			break
		}

		attempts.Go(func() {
			defer connections.Release(1)

			client, err := manager.connect(ctx, config)
			if err != nil {
				log.Printf("Error connecting to proxy %s:%d: %v", config.host, config.port, err)
				return
			}

			log.Printf("Proxy %s ready", client.id)

			readyLock.Lock()
			defer readyLock.Unlock()

			ready = append(ready, client)
			manager.publish(slices.Clone(ready))
		})
	}

	attempts.Wait()

	return nil
}
