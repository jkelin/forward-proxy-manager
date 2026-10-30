package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

func TestParseProxyList(t *testing.T) {
	first := ProxyConfig{host: "10.0.0.1", port: 1080, username: "user", password: "pass"}
	second := ProxyConfig{host: "192.168.1.255", port: 65535, username: "u2", password: "p:with:colons"}

	for _, testCase := range []struct {
		name     string
		body     string
		expected []ProxyConfig
	}{
		{"empty", "", []ProxyConfig{}},
		{"lf", "10.0.0.1:1080:user:pass\n192.168.1.255:65535:u2:p:with:colons\n", []ProxyConfig{first, second}},
		{"crlf", "10.0.0.1:1080:user:pass\r\n192.168.1.255:65535:u2:p:with:colons\r\n", []ProxyConfig{first, second}},
		{"no trailing newline", "10.0.0.1:1080:user:pass\n192.168.1.255:65535:u2:p:with:colons", []ProxyConfig{first, second}},
		{"mixed endings and blank lines", "\r\n10.0.0.1:1080:user:pass\r\n\n  \n192.168.1.255:65535:u2:p:with:colons\n\n", []ProxyConfig{first, second}},
		{"surrounding whitespace", "  10.0.0.1:1080:user:pass \t\n", []ProxyConfig{first}},
		{"missing password", "10.0.0.1:1080:user\n10.0.0.1:1080:user:pass\n", []ProxyConfig{first}},
		{"missing credentials", "10.0.0.1:1080\n", []ProxyConfig{}},
		{"empty username", "10.0.0.1:1080::pass\n", []ProxyConfig{}},
		{"octet out of range", "10.0.0.256:1080:user:pass\n", []ProxyConfig{}},
		{"three octets", "10.0.1:1080:user:pass\n", []ProxyConfig{}},
		{"dot wildcard rejected", "10x0x0x1:1080:user:pass\n", []ProxyConfig{}},
		{"port zero", "10.0.0.1:0:user:pass\n", []ProxyConfig{}},
		{"port too large", "10.0.0.1:65536:user:pass\n", []ProxyConfig{}},
		{"port too long", "10.0.0.1:123456:user:pass\n", []ProxyConfig{}},
		{"hostname", "proxy.example.com:1080:user:pass\n", []ProxyConfig{}},
		{"ipv6", "[::1]:1080:user:pass\n", []ProxyConfig{}},
		{"html error page", "<html>502 Bad Gateway</html>", []ProxyConfig{}},
		{"malformed between valid", "10.0.0.1:1080:user:pass\ngarbage\n192.168.1.255:65535:u2:p:with:colons\n", []ProxyConfig{first, second}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if actual := parseProxyList(testCase.body); !reflect.DeepEqual(actual, testCase.expected) {
				t.Fatalf("got %+v, want %+v", actual, testCase.expected)
			}
		})
	}
}

func TestFetchProxyList(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		code      int
		body      string
		expected  int
		wantError bool
	}{
		{"ok", http.StatusOK, "10.0.0.1:1080:user:pass\r\n10.0.0.2:1080:user:pass\r\n", 2, false},
		{"ok empty", http.StatusOK, "", 0, false},
		{"server error", http.StatusInternalServerError, "10.0.0.1:1080:user:pass\n", 0, true},
		{"not found", http.StatusNotFound, "", 0, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(testCase.code)
				_, _ = io.WriteString(writer, testCase.body)
			}))

			previous := http.DefaultClient
			http.DefaultClient = server.Client()
			t.Cleanup(func() { http.DefaultClient = previous })

			configs, err := fetchProxyList(context.Background(), server.URL)
			if (err != nil) != testCase.wantError {
				t.Fatalf("error %v, wantError %v", err, testCase.wantError)
			}

			if len(configs) != testCase.expected {
				t.Fatalf("got %d proxies, want %d", len(configs), testCase.expected)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		if _, err := fetchProxyList(context.Background(), "http://"+closedPort(t)+"/list"); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("invalid url", func(t *testing.T) {
		if _, err := fetchProxyList(context.Background(), "://nope"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func socksError(inner error) error {
	return &url.Error{Op: "Get", URL: "https://example.com", Err: &net.OpError{Op: "socks connect", Net: "tcp", Err: inner}}
}

func TestClassifyTransportError(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		err      error
		expected ResponseStatus
		known    bool
	}{
		{"deadline", context.DeadlineExceeded, ResponseStatusTimeout, true},
		{"wrapped deadline", fmt.Errorf("get: %w", context.DeadlineExceeded), ResponseStatusTimeout, true},
		{"net timeout", &url.Error{Op: "Get", URL: "x", Err: timeoutError{}}, ResponseStatusTimeout, true},
		{"socks timeout", socksError(timeoutError{}), ResponseStatusTimeout, true},
		{"proxy refused", socksError(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), ResponseStatusProxyUnreachable, true},
		{"proxy auth", socksError(errors.New("username/password authentication failed")), ResponseStatusProxyUnreachable, true},
		{"proxy general failure", socksError(errors.New("unknown error general SOCKS server failure")), ResponseStatusProxyUnreachable, true},
		{"proxy ruleset", socksError(errors.New("unknown error connection not allowed by ruleset")), ResponseStatusProxyUnreachable, true},
		{"socks without inner error", socksError(nil), ResponseStatusProxyUnreachable, true},
		{"target host unreachable", socksError(errors.New("unknown error host unreachable")), ResponseStatusHostUnreachable, true},
		{"target network unreachable", socksError(errors.New("unknown error network unreachable")), ResponseStatusHostUnreachable, true},
		{"target refused", socksError(errors.New("unknown error connection refused")), ResponseStatusHostUnreachable, true},
		{"target ttl", socksError(errors.New("unknown error TTL expired")), ResponseStatusHostUnreachable, true},
		{"other op", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, 0, false},
		{"plain", errors.New("boom"), 0, false},
		{"eof", io.EOF, 0, false},
		{"cancelled", context.Canceled, 0, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, known := classifyTransportError(testCase.err)
			if status != testCase.expected || known != testCase.known {
				t.Fatalf("got (%d, %v), want (%d, %v)", status, known, testCase.expected, testCase.known)
			}
		})
	}
}

func TestHandleError(t *testing.T) {
	client := &ProxyClient{id: "test"}
	target := mustParseUrl(t, "https://example.com/")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if response, err := client.handleError(cancelled, "GET", target, context.DeadlineExceeded); response != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("done caller ctx must win: %+v %v", response, err)
	}

	if response, err := client.handleError(context.Background(), "GET", target, context.DeadlineExceeded); err != nil || response.Status != ResponseStatusTimeout {
		t.Fatalf("timeout: %+v %v", response, err)
	}

	unknown := errors.New("boom")
	if response, err := client.handleError(context.Background(), "GET", target, unknown); response != nil || err != unknown {
		t.Fatalf("unknown errors pass through: %+v %v", response, err)
	}
}

// fakeClock is a manually advanced clock safe for concurrent use.
type fakeClock struct {
	lock sync.Mutex
	now  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (clock *fakeClock) Now() time.Time {
	clock.lock.Lock()
	defer clock.lock.Unlock()

	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.lock.Lock()
	defer clock.lock.Unlock()

	clock.now = clock.now.Add(duration)
}

func newLimitedClient(t *testing.T, clock *fakeClock) *ProxyClient {
	t.Helper()

	limits := exitLimits{
		requestsPerMin:     60,
		burst:              2,
		unreachableRetry:   time.Minute,
		rateLimitedBackoff: 30 * time.Second,
		clock:              clock.Now,
	}

	client, err := newProxyClient("limited", profiles.Chrome_144, limits)
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func rateLimited(t *testing.T, client *ProxyClient, host string) (bool, time.Duration) {
	t.Helper()

	limited, result, err := client.RateLimit(HostInfo{host: host})
	if err != nil {
		t.Fatal(err)
	}

	return limited, result.RetryAfter
}

func TestRateLimitBurstAndRefill(t *testing.T) {
	clock := newFakeClock()
	client := newLimitedClient(t, clock)

	// Burst 2 allows 3 immediate requests (GCRA counts the emission slot).
	for attempt := range 3 {
		if limited, _ := rateLimited(t, client, "a.test"); limited {
			t.Fatalf("attempt %d limited", attempt)
		}
	}

	limited, retryAfter := rateLimited(t, client, "a.test")
	if !limited || retryAfter <= 0 || retryAfter > time.Second {
		t.Fatalf("expected limit with retry within 1s, got %v %v", limited, retryAfter)
	}

	if limited, _ := rateLimited(t, client, "b.test"); limited {
		t.Fatal("limits are per host")
	}

	clock.Advance(time.Second)
	if limited, _ := rateLimited(t, client, "a.test"); limited {
		t.Fatal("token must refill after one emission interval")
	}
}

func TestRateLimitUnreachable(t *testing.T) {
	clock := newFakeClock()
	client := newLimitedClient(t, clock)

	client.markUnreachable()

	limited, retryAfter := rateLimited(t, client, "a.test")
	if !limited || retryAfter != time.Minute {
		t.Fatalf("unreachable exit: limited %v retry %v", limited, retryAfter)
	}

	clock.Advance(45 * time.Second)
	if limited, retryAfter := rateLimited(t, client, "b.test"); !limited || retryAfter != 15*time.Second {
		t.Fatalf("unreachable applies to every host with the remaining wait: %v %v", limited, retryAfter)
	}

	clock.Advance(15 * time.Second)

	// The unreachable window consumed no tokens, so the full burst is available.
	for attempt := range 3 {
		if limited, _ := rateLimited(t, client, "a.test"); limited {
			t.Fatalf("attempt %d limited after window", attempt)
		}
	}
}

func TestRateLimitBackOff(t *testing.T) {
	clock := newFakeClock()
	client := newLimitedClient(t, clock)

	client.backOff(HostInfo{host: "a.test"})

	if limited, retryAfter := rateLimited(t, client, "a.test"); !limited || retryAfter != 30*time.Second {
		t.Fatalf("backed-off host: %v %v", limited, retryAfter)
	}

	if limited, _ := rateLimited(t, client, "b.test"); limited {
		t.Fatal("back-off is per host")
	}

	clock.Advance(30 * time.Second)
	if limited, _ := rateLimited(t, client, "a.test"); limited {
		t.Fatal("back-off must expire")
	}

	client.availabilityLock.Lock()
	_, kept := client.backoffUntil["a.test"]
	client.availabilityLock.Unlock()

	if kept {
		t.Fatal("expired back-off must be forgotten")
	}
}

func TestRateLimitConcurrent(t *testing.T) {
	client := newLimitedClient(t, newFakeClock())

	var group sync.WaitGroup
	for index := range 50 {
		group.Go(func() {
			host := HostInfo{host: fmt.Sprintf("h%d.test", index%5)}
			if index%7 == 0 {
				client.markUnreachable()
			}

			if index%3 == 0 {
				client.backOff(host)
			}

			if _, _, err := client.RateLimit(host); err != nil {
				t.Error(err)
			}
		})
	}

	group.Wait()
}

func TestNewLimiterValidation(t *testing.T) {
	for _, testCase := range []struct {
		perMin  int
		burst   int
		wantErr bool
	}{
		{30, 5, false},
		{1, 0, false},
		{0, 5, true},
		{-1, 5, true},
		{30, -1, true},
	} {
		_, err := newLimiter(testCase.perMin, testCase.burst, time.Now)
		if (err != nil) != testCase.wantErr {
			t.Errorf("newLimiter(%d, %d): error %v", testCase.perMin, testCase.burst, err)
		}
	}

	limits := testLimits()
	limits.requestsPerMin = 0
	if _, err := newProxyClient("x", profiles.Chrome_144, limits); err == nil {
		t.Fatal("newProxyClient must reject invalid limits")
	}
}

func gzipBytes(t *testing.T, payload string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func TestProxyClientThroughSocks(t *testing.T) {
	type observed struct {
		proto     string
		userAgent string
		host      string
	}

	var lock sync.Mutex
	seen := make([]observed, 0)

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lock.Lock()
		seen = append(seen, observed{proto: request.Proto, userAgent: request.UserAgent(), host: request.Host})
		lock.Unlock()

		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Content-Disposition", "inline")
		writer.Header().Set("X-Secret", "dropped")

		if request.URL.Path == "/gzip" {
			writer.Header().Set("Content-Encoding", "gzip")
			_, _ = writer.Write(gzipBytes(t, "<p>compressed</p>"))

			return
		}

		writer.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(writer, "<p>plain</p>")
	})

	tlsTarget := startTargetServer(t, handler)
	socks := startSocksServer(t, "user", "secret")
	client := newSocksProxyClient(t, socks, testLimits())

	for _, testCase := range []struct {
		name  string
		path  string
		code  int
		body  string
		proto string
	}{
		{"https h2", "/page", http.StatusAccepted, "<p>plain</p>", "HTTP/2.0"},
		{"https gzip", "/gzip", http.StatusOK, "<p>compressed</p>", "HTTP/2.0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			original := tlsTarget.URL + testCase.path
			target := mustParseUrl(t, original)
			request := &ActiveRequest{
				Url:     target,
				Method:  http.MethodGet,
				Host:    HostInfo{host: target.Hostname()},
				Context: context.Background(),
			}

			response, err := client.makeRequestWithClient(request, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}

			if response.Status != ResponseStatusOk || response.Code != testCase.code || string(response.Body) != testCase.body {
				t.Fatalf("unexpected response %+v (%q)", response, response.Body)
			}

			expectedHeaders := http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Content-Disposition": {"inline"}}
			if !reflect.DeepEqual(response.Headers, expectedHeaders) {
				t.Fatalf("only content headers pass back, got %v", response.Headers)
			}

			if target.String() != original {
				t.Fatalf("request URL mutated to %s", target)
			}

			lock.Lock()
			last := seen[len(seen)-1]
			lock.Unlock()

			if last.proto != testCase.proto || !strings.Contains(last.userAgent, "Chrome/144") {
				t.Fatalf("not impersonating Chrome over h2: %+v", last)
			}
		})
	}

	if targets := socks.connectedTargets(); len(targets) == 0 || targets[0] != strings.TrimPrefix(tlsTarget.URL, "https://") {
		t.Fatalf("traffic must go through the SOCKS proxy, saw %v", targets)
	}
}

func TestProxyClientPlainHttpThroughSocks(t *testing.T) {
	target := startPlainServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, request.Proto)
	}))
	socks := startSocksServer(t, "user", "secret")
	client := newSocksProxyClient(t, socks, testLimits())

	uri := mustParseUrl(t, target.URL+"/")
	response, err := client.makeRequestWithClient(&ActiveRequest{
		Url: uri, Method: http.MethodGet, Host: HostInfo{host: uri.Hostname()}, Context: context.Background(),
	}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || string(response.Body) != "HTTP/1.1" {
		t.Fatalf("unexpected %d %q", response.Code, response.Body)
	}
}

func TestProxyClientFailureMapping(t *testing.T) {
	slow := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	fast := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "ok")
	}))

	for _, testCase := range []struct {
		name     string
		setup    func(t *testing.T) (*ProxyClient, string)
		timeout  time.Duration
		expected ResponseStatus
	}{
		{
			name: "proxy down",
			setup: func(t *testing.T) (*ProxyClient, string) {
				socks := startSocksServer(t, "user", "secret")
				_ = socks.listener.Close()

				return newSocksProxyClient(t, socks, testLimits()), fast.URL
			},
			timeout:  5 * time.Second,
			expected: ResponseStatusProxyUnreachable,
		},
		{
			name: "wrong proxy password",
			setup: func(t *testing.T) (*ProxyClient, string) {
				socks := startSocksServer(t, "user", "secret")

				return newSocksProxyClientWithPassword(t, socks, "other", testLimits()), fast.URL
			},
			timeout:  5 * time.Second,
			expected: ResponseStatusProxyUnreachable,
		},
		{
			name: "proxy general failure",
			setup: func(t *testing.T) (*ProxyClient, string) {
				socks := startSocksServer(t, "user", "secret")
				socks.forcedReply.Store(uint32(socksGeneralFailure))

				return newSocksProxyClient(t, socks, testLimits()), fast.URL
			},
			timeout:  5 * time.Second,
			expected: ResponseStatusProxyUnreachable,
		},
		{
			name: "target host unreachable",
			setup: func(t *testing.T) (*ProxyClient, string) {
				socks := startSocksServer(t, "user", "secret")
				socks.forcedReply.Store(uint32(socksHostUnreachable))

				return newSocksProxyClient(t, socks, testLimits()), fast.URL
			},
			timeout:  5 * time.Second,
			expected: ResponseStatusHostUnreachable,
		},
		{
			name: "target refuses",
			setup: func(t *testing.T) (*ProxyClient, string) {
				socks := startSocksServer(t, "user", "secret")

				return newSocksProxyClient(t, socks, testLimits()), "https://" + closedPort(t)
			},
			timeout:  5 * time.Second,
			expected: ResponseStatusHostUnreachable,
		},
		{
			name: "target too slow",
			setup: func(t *testing.T) (*ProxyClient, string) {
				socks := startSocksServer(t, "user", "secret")

				return newSocksProxyClient(t, socks, testLimits()), slow.URL
			},
			timeout:  300 * time.Millisecond,
			expected: ResponseStatusTimeout,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, rawUrl := testCase.setup(t)
			uri := mustParseUrl(t, rawUrl+"/")

			response, err := client.makeRequestWithClient(&ActiveRequest{
				Url: uri, Method: http.MethodGet, Host: HostInfo{host: uri.Hostname()}, Context: context.Background(),
			}, testCase.timeout)
			if err != nil {
				t.Fatalf("expected a status, got error %v", err)
			}

			if response.Status != testCase.expected {
				t.Fatalf("got status %d, want %d", response.Status, testCase.expected)
			}
		})
	}

	t.Run("caller cancelled", func(t *testing.T) {
		socks := startSocksServer(t, "user", "secret")
		client := newSocksProxyClient(t, socks, testLimits())
		uri := mustParseUrl(t, slow.URL+"/")

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)

		response, err := client.makeRequestWithClient(&ActiveRequest{
			Url: uri, Method: http.MethodGet, Host: HostInfo{host: uri.Hostname()}, Context: ctx,
		}, 5*time.Second)
		if response != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %+v %v", response, err)
		}
	})
}

func TestGetExternalProxyIp(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		code     int
		body     string
		expected string
		wantErr  bool
	}{
		{"ipv4 with newline", http.StatusOK, "203.0.113.7\n", "203.0.113.7", false},
		{"ipv6 with spaces", http.StatusOK, "  2001:db8::1 \r\n", "2001:db8::1", false},
		{"not an ip", http.StatusOK, "<html>captcha</html>", "", true},
		{"empty", http.StatusOK, "", "", true},
		{"server error", http.StatusInternalServerError, "203.0.113.7", "", true},
		{"redirect", http.StatusFound, "203.0.113.7", "", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(testCase.code)
				_, _ = io.WriteString(writer, testCase.body)
			}))
			client := newSocksProxyClient(t, startSocksServer(t, "user", "secret"), testLimits())

			ip, err := getExternalProxyIp(context.Background(), client, mustParseUrl(t, server.URL+"/ip"), 5*time.Second)
			if (err != nil) != testCase.wantErr || ip != testCase.expected {
				t.Fatalf("got %q %v", ip, err)
			}
		})
	}

	t.Run("proxy down", func(t *testing.T) {
		socks := startSocksServer(t, "user", "secret")
		_ = socks.listener.Close()
		client := newSocksProxyClient(t, socks, testLimits())

		if _, err := getExternalProxyIp(context.Background(), client, mustParseUrl(t, "https://127.0.0.1:1/ip"), time.Second); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestProxyManagerRun(t *testing.T) {
	ipServer := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "198.51.100.9\n")
	}))

	healthy := []*socksServer{startSocksServer(t, "a", "1"), startSocksServer(t, "b", "2:x")}
	down := startSocksServer(t, "c", "3")
	_ = down.listener.Close()
	wrongPassword := startSocksServer(t, "d", "changed")

	lines := make([]string, 0)
	for _, socks := range []*socksServer{healthy[0], down, healthy[1]} {
		config := socks.config()
		lines = append(lines, fmt.Sprintf("%s:%d:%s:%s", config.host, config.port, config.username, config.password))
	}

	wrongConfig := wrongPassword.config()
	lines = append(lines, "not a proxy", fmt.Sprintf("%s:%d:%s:%s", wrongConfig.host, wrongConfig.port, wrongConfig.username, "4"))

	listServer := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, strings.Join(lines, "\r\n"))
	}))

	previous := http.DefaultClient
	http.DefaultClient = listServer.Client()
	t.Cleanup(func() { http.DefaultClient = previous })

	var publishLock sync.Mutex
	published := make([][]proxyExit, 0)

	manager := &proxyManager{
		listUrl:        listServer.URL,
		ipCheckUrl:     mustParseUrl(t, ipServer.URL+"/ip"),
		ipCheckTimeout: 5 * time.Second,
		limits:         testLimits(),
		newProfile:     randomBrowserProfile,
		clientOptions:  []tls_client.HttpClientOption{tls_client.WithInsecureSkipVerify()},
		publish: func(proxies []proxyExit) {
			publishLock.Lock()
			defer publishLock.Unlock()

			published = append(published, proxies)
		},
		maxConcurrency: 2,
	}

	if err := manager.run(context.Background()); err != nil {
		t.Fatal(err)
	}

	publishLock.Lock()
	defer publishLock.Unlock()

	if len(published) != 2 || len(published[0]) != 1 || len(published[1]) != 2 {
		t.Fatalf("expected growing lists of 1 then 2 exits, got %v", published)
	}

	if !slices.Contains(published[1], published[0][0]) {
		t.Fatal("later lists must keep earlier exits")
	}

	for _, exit := range published[1] {
		if exit.(*ProxyClient).id != "198.51.100.9" {
			t.Fatalf("exit must be named after its public IP, got %q", exit.(*ProxyClient).id)
		}
	}

	for _, socks := range healthy {
		if socks.connects.Load() == 0 {
			t.Fatal("every healthy proxy must be used for the IP check")
		}
	}
}

func TestProxyManagerRunListFailure(t *testing.T) {
	manager := &proxyManager{listUrl: "http://" + closedPort(t) + "/list", maxConcurrency: 1}

	if err := manager.run(context.Background()); err == nil {
		t.Fatal("a failing list download must be returned")
	}
}

func TestProxyManagerRunCancelled(t *testing.T) {
	listServer := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "10.0.0.1:1080:a:b\n")
	}))

	previous := http.DefaultClient
	http.DefaultClient = listServer.Client()
	t.Cleanup(func() { http.DefaultClient = previous })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	manager := &proxyManager{listUrl: listServer.URL, maxConcurrency: 1, publish: func([]proxyExit) { t.Error("nothing may be published") }}
	if err := manager.run(ctx); err == nil {
		t.Fatal("a cancelled ctx aborts the list download")
	}
}
