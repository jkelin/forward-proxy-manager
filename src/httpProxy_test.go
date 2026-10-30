package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-httpproxy/httpproxy"
)

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()

	return string(body)
}

func TestParsePriority(t *testing.T) {
	for _, testCase := range []struct {
		raw      string
		expected int64
	}{
		{"", 0},
		{"0", 0},
		{"7", 7},
		{"-4", -4},
		{"+3", 3},
		{"2147483647", 2147483647},
		{"-2147483648", -2147483648},
		{"2147483648", 0},
		{"1.5", 0},
		{"high", 0},
		{" 5", 0},
	} {
		if actual := parsePriority(testCase.raw); actual != testCase.expected {
			t.Errorf("parsePriority(%q) = %d, want %d", testCase.raw, actual, testCase.expected)
		}
	}
}

func TestCreateStringResp(t *testing.T) {
	response := createStringResp("hello", http.StatusTeapot)

	if response.StatusCode != http.StatusTeapot || response.ContentLength != 5 || response.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("unexpected response %+v", response)
	}

	if body := readResponse(t, response); body != "hello" {
		t.Fatalf("got body %q", body)
	}
}

func TestOnConnectAlwaysMitm(t *testing.T) {
	for _, host := range []string{"a.test:443", "127.0.0.1:8443", "[::1]:443"} {
		action, newHost := OnConnect(nil, host)
		if action != httpproxy.ConnectMitm || newHost != host {
			t.Errorf("OnConnect(%q) = %v, %q", host, action, newHost)
		}
	}
}

func TestOnErrorTolerates(t *testing.T) {
	for _, opErr := range []error{errors.New("request canceled"), errors.New("boom")} {
		OnError(nil, "Request", &httpproxy.Error{}, opErr)
	}
}

// proxiedRequest builds the absolute-URL request httpproxy hands to OnRequest.
func proxiedRequest(ctx context.Context, headers map[string]string) *http.Request {
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://a.test/page?q=1", nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	return request
}

func TestOnRequestResponses(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, testCase := range []struct {
		name    string
		respond func(*ActiveRequest, int) (*Response, error)
		ctx     context.Context
		headers map[string]string
		code    int
		body    string
	}{
		{"relative warmup", respondWith(200), nil, map[string]string{"x-warmup-url": "/warm"}, 400, "Invalid x-warmup-url"},
		{"ftp warmup", respondWith(200), nil, map[string]string{"x-warmup-url": "ftp://a.test/"}, 400, "Invalid x-warmup-url"},
		{"host-less warmup", respondWith(200), nil, map[string]string{"x-warmup-url": "https://:443/"}, 400, "Invalid x-warmup-url"},
		{"timeout", respondSequence(&Response{Status: ResponseStatusTimeout}), nil, nil, 502, "Remote host timed out"},
		{"host unreachable", respondSequence(&Response{Status: ResponseStatusHostUnreachable}), nil, nil, 502, "Remote host unreachable"},
		{"unknown status", respondSequence(&Response{Status: ResponseStatusUnknownError}), nil, nil, 500, "Proxy error"},
		{"unknown error", func(*ActiveRequest, int) (*Response, error) { return nil, errors.New("boom") }, nil, nil, 500, "Proxy error"},
		{"client gone", waitForContext, cancelled, nil, statusClientClosedRequest, "Request cancelled"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newRequestScheduler(0, time.Second)
			scheduler.idleInterval = 10 * time.Millisecond
			exit := newFakeExit("only", testCase.respond)
			startScheduler(t, scheduler, exit)

			ctx := testCase.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			handler := &httpProxyHandler{scheduler: scheduler}
			response := handler.OnRequest(nil, proxiedRequest(ctx, testCase.headers))

			if response.StatusCode != testCase.code {
				t.Fatalf("got %d, want %d", response.StatusCode, testCase.code)
			}

			if body := readResponse(t, response); !strings.HasPrefix(body, testCase.body) {
				t.Fatalf("got body %q, want prefix %q", body, testCase.body)
			}

			if testCase.code == 400 && exit.attemptCount() != 0 {
				t.Fatal("rejected requests must never reach an exit")
			}
		})
	}
}

func TestOnRequestSuccess(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondSequence(&Response{
		Status: ResponseStatusOk,
		Code:   http.StatusCreated,
		Body:   []byte("payload"),
		Headers: http.Header{
			"Content-Type":      {"application/json"},
			"Content-Length":    {"999"},
			"Transfer-Encoding": {"chunked"},
			"X-Fpm-Warmup":      {"reused"},
		},
	}))
	startScheduler(t, scheduler, exit)

	response := (&httpProxyHandler{scheduler: scheduler}).OnRequest(nil, proxiedRequest(context.Background(), nil))

	if response.StatusCode != http.StatusCreated || response.ContentLength != -1 || response.TransferEncoding != nil {
		t.Fatalf("unexpected response %+v", response)
	}

	expected := http.Header{"Content-Type": {"application/json"}, "X-Fpm-Warmup": {"reused"}}
	if len(response.Header) != len(expected) || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("X-Fpm-Warmup") != "reused" {
		t.Fatalf("got headers %v, want %v", response.Header, expected)
	}

	if body := readResponse(t, response); body != "payload" {
		t.Fatalf("got body %q", body)
	}
}

func TestOnRequestPassesControlHeaders(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		headers  map[string]string
		priority int64
		warmup   string
	}{
		{"none", nil, 0, ""},
		{"priority", map[string]string{"x-priority": "12"}, 12, ""},
		{"bad priority", map[string]string{"x-priority": "urgent"}, 0, ""},
		{"warmup", map[string]string{"x-warmup-url": "https://a.test/home"}, 0, "https://a.test/home"},
		{"canonical header names", map[string]string{"X-Priority": "-2", "X-Warmup-Url": "http://a.test/"}, -2, "http://a.test/"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(0)
			exit := newFakeExit("only", respondWith(200))
			startScheduler(t, scheduler, exit)

			response := (&httpProxyHandler{scheduler: scheduler}).OnRequest(nil, proxiedRequest(context.Background(), testCase.headers))
			_ = readResponse(t, response)

			request := exit.attempt(0)
			if request.Priority != testCase.priority || request.Url.String() != "http://a.test/page?q=1" || len(request.RetryOnCodes) != 0 {
				t.Fatalf("unexpected request %+v", request)
			}

			if (request.WarmupUrl == nil) != (testCase.warmup == "") || (request.WarmupUrl != nil && request.WarmupUrl.String() != testCase.warmup) {
				t.Fatalf("got warmup %v, want %q", request.WarmupUrl, testCase.warmup)
			}
		})
	}
}

// startHttpProxy serves newHttpProxy(scheduler) and returns a client that uses it, trusting its MITM certificates.
func startHttpProxy(t *testing.T, scheduler *RequestScheduler) *http.Client {
	t.Helper()

	proxy, err := newHttpProxy(scheduler)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	proxyUrl, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyUrl),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func getThrough(t *testing.T, client *http.Client, target string, headers map[string]string) (*http.Response, string) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}

	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	return response, readResponse(t, response)
}

func TestHttpProxyPlainEndToEnd(t *testing.T) {
	var lock sync.Mutex
	var received http.Header

	target := startPlainServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lock.Lock()
		received = request.Header.Clone()
		lock.Unlock()

		writer.Header().Set("Content-Type", "text/html")
		writer.Header().Set("X-Internal", "dropped")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte("plain " + request.URL.RequestURI()))
	}))

	socks := startSocksServer(t, "user", "secret")
	scheduler := newRequestScheduler(0, 5*time.Second)
	scheduler.idleInterval = 10 * time.Millisecond
	startScheduler(t, scheduler, newSocksProxyClient(t, socks, testLimits()))

	response, body := getThrough(t, startHttpProxy(t, scheduler), target.URL+"/a?b=c", map[string]string{
		"x-priority": "3",
		"X-Secret":   "caller-only",
	})

	if response.StatusCode != http.StatusAccepted || body != "plain /a?b=c" || response.Header.Get("Content-Type") != "text/html" {
		t.Fatalf("got %d %q %v", response.StatusCode, body, response.Header)
	}

	if response.Header.Get("X-Internal") != "" {
		t.Fatal("only Content-Type and Content-Disposition are passed back")
	}

	lock.Lock()
	defer lock.Unlock()

	for _, leaked := range []string{"X-Priority", "X-Secret", "X-Warmup-Url"} {
		if received.Get(leaked) != "" {
			t.Errorf("caller header %s leaked to the target", leaked)
		}
	}

	if userAgent := received.Get("User-Agent"); !strings.Contains(userAgent, "Mozilla/5.0") {
		t.Errorf("target must see the browser identity, got User-Agent %q", userAgent)
	}

	if targets := socks.connectedTargets(); len(targets) != 1 || targets[0] != strings.TrimPrefix(target.URL, "http://") {
		t.Errorf("the request must go through the SOCKS exit, got %v", targets)
	}
}

func TestHttpProxyMitmWarmupEndToEnd(t *testing.T) {
	socks, base := warmupSite(t, http.StatusOK)
	scheduler := newRequestScheduler(0, 5*time.Second)
	scheduler.idleInterval = 10 * time.Millisecond
	startScheduler(t, scheduler, newSocksProxyClient(t, socks, testLimits()))
	client := startHttpProxy(t, scheduler)

	response, _ := getThrough(t, client, base+"/search", nil)
	if response.StatusCode != http.StatusForbidden || response.Header.Get("X-Fpm-Warmup") != "" {
		t.Fatalf("without x-warmup-url no session is made: %d %v", response.StatusCode, response.Header)
	}

	for _, expected := range []string{"fresh", "reused"} {
		response, body := getThrough(t, client, base+"/search", map[string]string{"x-warmup-url": base + "/warm"})
		if response.StatusCode != http.StatusOK || body != "results" || response.Header.Get("X-Fpm-Warmup") != expected {
			t.Fatalf("want %s: got %d %q %v", expected, response.StatusCode, body, response.Header)
		}
	}
}

func TestHttpProxyMitmWarmupFailed(t *testing.T) {
	socks, base := warmupSite(t, http.StatusServiceUnavailable)
	scheduler := newRequestScheduler(0, 5*time.Second)
	scheduler.idleInterval = 10 * time.Millisecond
	startScheduler(t, scheduler, newSocksProxyClient(t, socks, testLimits()))

	response, body := getThrough(t, startHttpProxy(t, scheduler), base+"/search", map[string]string{"x-warmup-url": base + "/warm"})
	if response.StatusCode != http.StatusServiceUnavailable || body != "warm-up blocked" || response.Header.Get("X-Fpm-Warmup") != "failed" {
		t.Fatalf("got %d %q %v", response.StatusCode, body, response.Header)
	}
}

func TestHttpProxyInvalidWarmupOverWire(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondWith(200))
	startScheduler(t, scheduler, exit)

	response, body := getThrough(t, startHttpProxy(t, scheduler), "http://a.test/", map[string]string{"x-warmup-url": "not a url"})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "x-warmup-url") {
		t.Fatalf("got %d %q", response.StatusCode, body)
	}

	if exit.attemptCount() != 0 {
		t.Fatal("rejected requests must never reach an exit")
	}
}
