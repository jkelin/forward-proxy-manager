package main

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

func mustParseUrl(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	return parsed
}

func TestFetchSiteAndReferer(t *testing.T) {
	for _, testCase := range []struct {
		initiator string
		target    string
		site      string
		referer   string
	}{
		{"https://www.ebay.com/n/all-categories#top", "https://www.ebay.com/sch/i.html?_nkw=xeon", "same-origin", "https://www.ebay.com/n/all-categories"},
		{"https://www.ebay.com/n/all-categories", "https://pages.ebay.com/help", "same-site", "https://www.ebay.com/"},
		{"https://www.ebay.com/n/all-categories", "https://www.ebay.de/sch/i.html", "cross-site", "https://www.ebay.com/"},
		{"http://www.ebay.com/n/all-categories", "https://www.ebay.com/sch/i.html", "cross-site", "http://www.ebay.com/"},
		{"https://www.ebay.com/n/all-categories", "http://www.ebay.com/sch/i.html", "cross-site", ""},
		{"https://shop.example.co.uk/", "https://example.co.uk/", "same-site", "https://shop.example.co.uk/"},
		{"https://a.co.uk/", "https://b.co.uk/", "cross-site", "https://a.co.uk/"},
		{"https://127.0.0.1:8443/warm", "https://127.0.0.1:8443/search", "same-origin", "https://127.0.0.1:8443/warm"},
		{"https://127.0.0.1:8443/warm", "https://127.0.0.2:8443/search", "cross-site", "https://127.0.0.1:8443/"},
	} {
		initiator := mustParseUrl(t, testCase.initiator)
		target := mustParseUrl(t, testCase.target)

		if site := fetchSite(initiator, target); site != testCase.site {
			t.Errorf("fetchSite(%s, %s) = %s, want %s", testCase.initiator, testCase.target, site, testCase.site)
		}

		if referer := refererFor(initiator, target); referer != testCase.referer {
			t.Errorf("refererFor(%s, %s) = %q, want %q", testCase.initiator, testCase.target, referer, testCase.referer)
		}
	}
}

func TestNavigationHeaders(t *testing.T) {
	initiator := mustParseUrl(t, "https://www.ebay.com/n/all-categories")
	target := mustParseUrl(t, "https://www.ebay.com/sch/i.html?_nkw=xeon")

	for _, profile := range browserProfiles {
		t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
			base := getFakeHeaders(profile)
			headers := navigationHeaders(base, initiator, target)

			if headerValue(headers, "referer") != initiator.String() || headerValue(headers, "sec-fetch-site") != "same-origin" {
				t.Fatalf("unexpected navigation headers: %v", headers)
			}

			if headerValue(base, "sec-fetch-site") != "none" || headerValue(base, "referer") != "" {
				t.Fatal("navigationHeaders mutated the identity headers")
			}
		})
	}

	// A downgrade sends no Referer at all, like strict-origin-when-cross-origin.
	headers := navigationHeaders(getFakeHeaders(profiles.Chrome_144), initiator, mustParseUrl(t, "http://www.ebay.com/"))
	if _, present := headers["referer"]; present {
		t.Fatal("referer must be omitted on https -> http")
	}
}

func TestSessionJar(t *testing.T) {
	jar := newSessionJar()
	target := mustParseUrl(t, "https://www.ebay.com/sch/i.html")

	jar.SetCookies(target, []*http.Cookie{
		{Name: "live", Value: "1", Path: "/"},
		{Name: "expired", Value: "1", Path: "/", Expires: time.Now().Add(-time.Hour)},
	})

	cookies := jar.Cookies(target)
	if len(cookies) != 1 || cookies[0].Name != "live" {
		t.Fatalf("expected only the live cookie, got %v", cookies)
	}

	if len(jar.Cookies(mustParseUrl(t, "https://www.amazon.com/"))) != 0 {
		t.Fatal("cookies leaked to another host")
	}

	jar.Reset()
	if len(jar.Cookies(target)) != 0 {
		t.Fatal("reset must clear the jar")
	}
}

func TestIsSessionRejected(t *testing.T) {
	for _, testCase := range []struct {
		code      int
		hasWarmup bool
		expected  bool
	}{
		{403, true, true},
		{403, false, false},
		{200, true, false},
		{429, true, false},
	} {
		if actual := isSessionRejected(testCase.code, testCase.hasWarmup); actual != testCase.expected {
			t.Errorf("isSessionRejected(%d, %t) = %t", testCase.code, testCase.hasWarmup, actual)
		}
	}
}

// warmupServer accepts /search only with the cookie set by /warm plus browser navigation headers.
// /blocked always rejects; /warm-blocked rejects the warm-up itself. Both 403s set a cookie, like Akamai,
// so a non-empty jar alone must not count as warmed.
type warmupServer struct {
	server   *httptest.Server
	warmHits atomic.Int32
}

func newWarmupServer(t *testing.T) *warmupServer {
	state := &warmupServer{}
	mux := stdhttp.NewServeMux()

	mux.HandleFunc("/warm", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		state.warmHits.Add(1)
		if request.Header.Get("Sec-Fetch-Site") != "none" || request.Header.Get("Referer") != "" {
			writer.WriteHeader(stdhttp.StatusBadRequest)
			return
		}

		stdhttp.SetCookie(writer, &stdhttp.Cookie{Name: "session", Value: "ok", Path: "/"})
		_, _ = writer.Write([]byte("warm"))
	})

	reject := func(writer stdhttp.ResponseWriter) {
		stdhttp.SetCookie(writer, &stdhttp.Cookie{Name: "bm", Value: "tracked", Path: "/"})
		writer.WriteHeader(stdhttp.StatusForbidden)
	}

	mux.HandleFunc("/warm-blocked", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		state.warmHits.Add(1)
		reject(writer)
	})

	mux.HandleFunc("/blocked", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		reject(writer)
	})

	mux.HandleFunc("/search", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		cookie, err := request.Cookie("session")
		validNavigation := request.Header.Get("Sec-Fetch-Site") == "same-origin" && request.Header.Get("Referer") == state.server.URL+"/warm"
		if err != nil || cookie.Value != "ok" || !validNavigation {
			reject(writer)
			return
		}

		_, _ = writer.Write([]byte("results"))
	})

	state.server = httptest.NewUnstartedServer(mux)
	state.server.EnableHTTP2 = true
	state.server.StartTLS()
	t.Cleanup(state.server.Close)

	return state
}

func newTestProxyClient(t *testing.T) *ProxyClient {
	client, err := newProxyClient("test", profiles.Chrome_144, nil, tls_client.WithInsecureSkipVerify())
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func (state *warmupServer) request(t *testing.T, path string, warmupPath string) *ActiveRequest {
	request := &ActiveRequest{
		Url:     mustParseUrl(t, state.server.URL+path),
		Method:  "GET",
		Host:    HostInfo{host: "127.0.0.1", supportsHttps: true},
		Context: context.Background(),
	}

	if warmupPath != "" {
		request.WarmupUrl = mustParseUrl(t, state.server.URL+warmupPath)
	}

	return request
}

func (state *warmupServer) send(t *testing.T, client *ProxyClient, path string, warmupPath string) *Response {
	t.Helper()

	response, err := client.makeRequestWithClient(state.request(t, path, warmupPath), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return response
}

func TestWarmupFlow(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// Without x-warmup-url nothing changes: no warm-up, no session headers.
	if response := state.send(t, client, "/search", ""); response.Code != 403 || state.warmHits.Load() != 0 || response.Headers.Get(warmupStatusHeader) != "" {
		t.Fatalf("plain request: code %d, warm hits %d", response.Code, state.warmHits.Load())
	}

	// The 403 above stored a "bm" cookie; the client must still warm up.
	response := state.send(t, client, "/search", "/warm")
	if response.Code != 200 || string(response.Body) != "results" || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 1 {
		t.Fatalf("first warm request: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}

	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "reused" || state.warmHits.Load() != 1 {
		t.Fatalf("second warm request: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}

	// A 403 with warm-up set discards the session, so the next request warms up again.
	if response := state.send(t, client, "/blocked", "/warm"); response.Code != 403 {
		t.Fatalf("blocked request: code %d", response.Code)
	}

	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 2 {
		t.Fatalf("after reset: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}
}

func TestWarmupFailure(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// A rejected warm-up returns the warm-up response and skips the real request.
	response := state.send(t, client, "/search", "/warm-blocked")
	if response.Code != 403 || response.Headers.Get(warmupStatusHeader) != "failed" || state.warmHits.Load() != 1 {
		t.Fatalf("failed warm-up: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}

	// The failed warm-up must not mark the host as warmed.
	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" {
		t.Fatalf("after failed warm-up: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}
}

func TestParseWarmupUrl(t *testing.T) {
	for raw, valid := range map[string]bool{
		"":                                       true,
		"https://www.ebay.com/n/all-categories":  true,
		"http://example.com/":                    true,
		"/n/all-categories":                      false,
		"ftp://example.com/":                     false,
		"https://":                               false,
		"https://www.ebay.com/%zz":               false,
	} {
		parsed, err := parseWarmupUrl(raw)
		if valid != (err == nil) {
			t.Errorf("parseWarmupUrl(%q) error = %v", raw, err)
		}

		if raw == "" && parsed != nil {
			t.Error("empty header must mean no warm-up")
		}
	}
}
