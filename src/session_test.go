package main

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
		// Non-default ports are part of the origin but not of the site.
		{"https://www.ebay.com:8443/a", "https://www.ebay.com/b", "same-site", "https://www.ebay.com:8443/"},
		{"http://localhost:3000/a", "http://localhost:4000/b", "same-site", "http://localhost:3000/"},
		{"https://[::1]/a", "https://[::1]:443/b", "same-origin", "https://[::1]/a"},
		{"http://www.ebay.com/a?x=1", "http://www.ebay.com/b", "same-origin", "http://www.ebay.com/a?x=1"},
		{"https://www.ebay.com/a?x=1#y%20z", "https://www.ebay.com/a", "same-origin", "https://www.ebay.com/a?x=1"},
		{"https://www.ebay.com/a", "https://ebay.com/", "same-site", "https://www.ebay.com/"},
		{"https://www.ebay.com/a", "http://pages.ebay.com/", "cross-site", ""},
		// github.io is a private public suffix, so sibling pages are different sites.
		{"https://foo.github.io/a", "https://bar.github.io/b", "cross-site", "https://foo.github.io/"},
		// Browsers never send URL credentials in Referer.
		{"https://user:secret@www.ebay.com/a?q=1#f", "https://www.ebay.com/b", "same-origin", "https://www.ebay.com/a?q=1"},
		{"https://user:secret@www.ebay.com/a", "https://pages.ebay.com/", "same-site", "https://www.ebay.com/"},
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

// DNS names are case-insensitive, so mixed-case URLs must not turn a same-origin navigation into a cross-site one.
func TestFetchSiteIgnoresHostCase(t *testing.T) {
	for _, testCase := range []struct {
		initiator string
		target    string
		site      string
	}{
		{"https://WWW.EBAY.COM/a", "https://www.ebay.com/b", "same-origin"},
		{"https://www.ebay.com/a", "https://Www.Ebay.Com/b", "same-origin"},
		{"https://SHOP.EBAY.COM/", "https://www.ebay.com/", "same-site"},
		{"https://SHOP.EBAY.COM/", "https://www.ebay.de/", "cross-site"},
	} {
		site := fetchSite(mustParseUrl(t, testCase.initiator), mustParseUrl(t, testCase.target))
		if site != testCase.site {
			t.Errorf("fetchSite(%s, %s) = %s, want %s", testCase.initiator, testCase.target, site, testCase.site)
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

// Each navigation gets its own header slices: editing one must not leak into the identity or a sibling navigation.
func TestNavigationHeadersIsolation(t *testing.T) {
	initiator := mustParseUrl(t, "https://www.ebay.com/n/all-categories")
	sameOrigin := mustParseUrl(t, "https://www.ebay.com/sch/i.html")
	crossSite := mustParseUrl(t, "https://www.ebay.de/sch/i.html")

	for _, profile := range browserProfiles {
		t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
			base := getFakeHeaders(profile)
			snapshot := base.Clone()

			first := navigationHeaders(base, initiator, sameOrigin)
			second := navigationHeaders(base, initiator, crossSite)

			first["user-agent"][0] = "bot"
			first[http.HeaderOrderKey][0] = "x-injected"
			first["referer"][0] = "https://evil.example/"
			delete(first, "accept")

			if !reflect.DeepEqual(base, snapshot) {
				t.Fatalf("navigation edits leaked into identity headers: %v", base)
			}

			if headerValue(second, "user-agent") != headerValue(snapshot, "user-agent") ||
				headerValue(second, "accept") != headerValue(snapshot, "accept") ||
				!reflect.DeepEqual(second[http.HeaderOrderKey], snapshot[http.HeaderOrderKey]) {
				t.Fatal("navigation edits leaked into a sibling navigation")
			}

			if headerValue(second, "referer") != "https://www.ebay.com/" || headerValue(second, "sec-fetch-site") != "cross-site" {
				t.Fatalf("unexpected cross-site navigation headers: %v", second)
			}
		})
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

func TestSessionJarScoping(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		origin string
		cookie http.Cookie
		target string
		sent   bool
	}{
		{"host-only on same host", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/"}, "https://www.ebay.com/sch/", true},
		{"host-only skips subdomain", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/"}, "https://pages.ebay.com/", false},
		{"domain reaches sibling", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/", Domain: "ebay.com"}, "https://pages.ebay.com/", true},
		{"domain reaches apex", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/", Domain: ".ebay.com"}, "https://ebay.com/", true},
		{"domain stays on site", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/", Domain: "ebay.com"}, "https://www.ebay.de/", false},
		{"public suffix domain rejected", "https://shop.example.co.uk/", http.Cookie{Name: "id", Value: "1", Path: "/", Domain: "co.uk"}, "https://shop.example.co.uk/", false},
		{"foreign domain rejected", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/", Domain: "amazon.com"}, "https://www.amazon.com/", false},
		{"ip domain attribute rejected", "https://127.0.0.1:8443/", http.Cookie{Name: "id", Value: "1", Path: "/", Domain: "127.0.0.1"}, "https://127.0.0.1:8443/", false},
		{"ip host-only ignores port", "https://127.0.0.1:8443/", http.Cookie{Name: "id", Value: "1", Path: "/"}, "https://127.0.0.1:9443/", true},
		{"path prefix matches", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/sch"}, "https://www.ebay.com/sch/i.html", true},
		{"path outside prefix", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/sch"}, "https://www.ebay.com/itm/1", false},
		{"secure on https", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/", Secure: true}, "https://www.ebay.com/", true},
		{"secure skips http", "https://www.ebay.com/", http.Cookie{Name: "id", Value: "1", Path: "/", Secure: true}, "http://www.ebay.com/", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			jar := newSessionJar()
			jar.SetCookies(mustParseUrl(t, testCase.origin), []*http.Cookie{&testCase.cookie})

			cookies := jar.Cookies(mustParseUrl(t, testCase.target))
			if sent := len(cookies) == 1 && cookies[0].Value == "1"; sent != testCase.sent || len(cookies) > 1 {
				t.Fatalf("cookies for %s: %v, want sent=%t", testCase.target, cookies, testCase.sent)
			}
		})
	}
}

func TestSessionJarReplacesAndDeletes(t *testing.T) {
	jar := newSessionJar()
	target := mustParseUrl(t, "https://www.ebay.com/")

	jar.SetCookies(target, []*http.Cookie{{Name: "session", Value: "1", Path: "/"}})
	jar.SetCookies(target, []*http.Cookie{{Name: "session", Value: "2", Path: "/"}})

	cookies := jar.Cookies(target)
	if len(cookies) != 1 || cookies[0].Value != "2" {
		t.Fatalf("expected the replaced cookie only, got %v", cookies)
	}

	jar.SetCookies(target, []*http.Cookie{{Name: "session", Path: "/", MaxAge: -1}})
	if cookies := jar.Cookies(target); len(cookies) != 0 {
		t.Fatalf("Max-Age<0 must delete the cookie, got %v", cookies)
	}
}

func TestSessionJarsAreIndependent(t *testing.T) {
	first := newSessionJar()
	second := newSessionJar()
	target := mustParseUrl(t, "https://www.ebay.com/")

	first.SetCookies(target, []*http.Cookie{{Name: "session", Value: "1", Path: "/"}})
	if len(second.Cookies(target)) != 0 {
		t.Fatal("cookies leaked between jars")
	}

	second.SetCookies(target, []*http.Cookie{{Name: "session", Value: "2", Path: "/"}})
	first.Reset()
	if cookies := second.Cookies(target); len(cookies) != 1 || cookies[0].Value != "2" {
		t.Fatalf("reset of one jar touched another: %v", cookies)
	}
}

// Run with -race: readers and writers must never touch a jar that Reset is replacing.
func TestSessionJarConcurrentReset(t *testing.T) {
	jar := newSessionJar()
	target := mustParseUrl(t, "https://www.ebay.com/")

	var group sync.WaitGroup
	for worker := range 8 {
		group.Go(func() {
			name := "worker" + strconv.Itoa(worker)
			for iteration := range 200 {
				jar.SetCookies(target, []*http.Cookie{{Name: name, Value: strconv.Itoa(iteration), Path: "/"}})
				for _, cookie := range jar.Cookies(target) {
					if !strings.HasPrefix(cookie.Name, "worker") {
						t.Errorf("unexpected cookie %v", cookie)
					}
				}
			}
		})
	}

	group.Go(func() {
		for range 200 {
			jar.Reset()
		}
	})

	group.Wait()

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
		{401, true, false},
		{500, true, false},
		{302, true, false},
	} {
		if actual := isSessionRejected(testCase.code, testCase.hasWarmup); actual != testCase.expected {
			t.Errorf("isSessionRejected(%d, %t) = %t", testCase.code, testCase.hasWarmup, actual)
		}
	}
}

// warmupServer accepts /search only with the cookie set by /warm plus browser navigation headers.
// /blocked always rejects; /warm-blocked rejects the warm-up itself. Both 403s set a cookie, like Akamai,
// so a non-empty jar alone must not count as warmed.
// /warm-empty succeeds without a cookie, /warm-error fails with 500 and /warm-slow hangs until the client gives up.
// /open always succeeds; /encoded sends a stacked "gzip, br" body and /corrupt claims gzip but sends plain text.
type warmupServer struct {
	server        *httptest.Server
	warmHits      atomic.Int32
	searchHits    atomic.Int32
	warmHeaders   atomic.Pointer[stdhttp.Header]
	searchHeaders atomic.Pointer[stdhttp.Header]
}

const encodedPayload = "<html>encoded — Xeon</html>"

func newWarmupServer(t *testing.T) *warmupServer {
	state := &warmupServer{}
	mux := stdhttp.NewServeMux()

	mux.HandleFunc("/warm", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		state.warmHits.Add(1)
		headers := request.Header.Clone()
		state.warmHeaders.Store(&headers)
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
		state.searchHits.Add(1)
		headers := request.Header.Clone()
		state.searchHeaders.Store(&headers)
		cookie, err := request.Cookie("session")
		validNavigation := request.Header.Get("Sec-Fetch-Site") == "same-origin" && request.Header.Get("Referer") == state.server.URL+"/warm"
		if err != nil || cookie.Value != "ok" || !validNavigation {
			reject(writer)
			return
		}

		_, _ = writer.Write([]byte("results"))
	})

	mux.HandleFunc("/warm-empty", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		state.warmHits.Add(1)
		_, _ = writer.Write([]byte("warm"))
	})

	mux.HandleFunc("/warm-error", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		state.warmHits.Add(1)
		writer.WriteHeader(stdhttp.StatusInternalServerError)
		_, _ = writer.Write([]byte("warm failed"))
	})

	release := make(chan struct{})
	mux.HandleFunc("/warm-slow", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		state.warmHits.Add(1)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	})

	mux.HandleFunc("/open", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		_, _ = writer.Write([]byte("open"))
	})

	encoded := encodeBody(t, []byte(encodedPayload), "gzip, br")
	mux.HandleFunc("/encoded", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Content-Encoding", "gzip, br")
		_, _ = writer.Write(encoded)
	})

	mux.HandleFunc("/corrupt", func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		_, _ = writer.Write([]byte(encodedPayload))
	})

	state.server = httptest.NewUnstartedServer(mux)
	state.server.EnableHTTP2 = true
	state.server.StartTLS()
	t.Cleanup(state.server.Close)
	// Cleanups run last-in first-out, so hung handlers are released before Close waits for them.
	t.Cleanup(func() { close(release) })

	return state
}

func newTestProxyClient(t *testing.T) *ProxyClient {
	return newProfileProxyClient(t, profiles.Chrome_144)
}

func newProfileProxyClient(t *testing.T, profile profiles.ClientProfile) *ProxyClient {
	client, err := newProxyClient("test", profile, testLimits(), tls_client.WithInsecureSkipVerify())
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

	// The real request never left, and the 403's tracking cookie went with the discarded session.
	if state.searchHits.Load() != 0 {
		t.Fatalf("real request sent after failed warm-up: %d", state.searchHits.Load())
	}

	if cookies := client.cookies.Cookies(mustParseUrl(t, state.server.URL+"/search")); len(cookies) != 0 {
		t.Fatalf("failed warm-up kept cookies: %v", cookies)
	}

	// The failed warm-up must not mark the host as warmed.
	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" {
		t.Fatalf("after failed warm-up: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}
}

func TestWarmupServerError(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// Any non-2xx warm-up fails, and the caller receives the warm-up page instead of the real one.
	response := state.send(t, client, "/search", "/warm-error")
	if response.Code != 500 || string(response.Body) != "warm failed" || response.Headers.Get(warmupStatusHeader) != "failed" {
		t.Fatalf("server error warm-up: code %d, body %q, status %q", response.Code, response.Body, response.Headers.Get(warmupStatusHeader))
	}

	if state.warmHits.Load() != 1 || state.searchHits.Load() != 0 {
		t.Fatalf("warm hits %d, search hits %d", state.warmHits.Load(), state.searchHits.Load())
	}

	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 2 {
		t.Fatalf("after server error: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}
}

func TestWarmupUnreachable(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// A closed loopback listener refuses the warm-up connection.
	closed := httptest.NewTLSServer(stdhttp.NotFoundHandler())
	closedUrl := closed.URL
	closed.Close()

	request := state.request(t, "/search", "")
	request.WarmupUrl = mustParseUrl(t, closedUrl+"/warm")
	if response, err := client.makeRequestWithClient(request, 10*time.Second); err == nil {
		t.Fatalf("unreachable warm-up must fail, got %+v", response)
	}

	if state.searchHits.Load() != 0 {
		t.Fatal("real request sent after unreachable warm-up")
	}

	response := state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" {
		t.Fatalf("after unreachable warm-up: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}
}

func TestWarmupTimeout(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// The request deadline covers the warm-up, so a hung warm-up page times the whole request out.
	response, err := client.makeRequestWithClient(state.request(t, "/search", "/warm-slow"), 300*time.Millisecond)
	if err != nil || response == nil || response.Status != ResponseStatusTimeout {
		t.Fatalf("hung warm-up: response %+v, error %v", response, err)
	}

	if state.searchHits.Load() != 0 {
		t.Fatal("real request sent after timed out warm-up")
	}

	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 2 {
		t.Fatalf("after timeout: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}
}

func TestWarmupWithoutCookiesRepeats(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// A warm-up page that sets no cookie leaves nothing to reuse, so every request warms up again.
	for attempt := 1; attempt <= 2; attempt++ {
		response := state.send(t, client, "/open", "/warm-empty")
		if response.Code != 200 || string(response.Body) != "open" || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != int32(attempt) {
			t.Fatalf("attempt %d: code %d, status %q, warm hits %d", attempt, response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
		}
	}
}

func TestWarmupExpiredCookieRewarms(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)
	target := mustParseUrl(t, state.server.URL+"/search")

	if response := state.send(t, client, "/search", "/warm"); response.Headers.Get(warmupStatusHeader) != "fresh" {
		t.Fatalf("first request: status %q", response.Headers.Get(warmupStatusHeader))
	}

	// Expire the session cookie as the server would; the host stays marked but the jar is empty.
	client.cookies.SetCookies(target, []*http.Cookie{{Name: "session", Path: "/", MaxAge: -1}})

	response := state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 2 {
		t.Fatalf("after expiry: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}
}

func TestRejectedRequestDropsSession(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)
	target := mustParseUrl(t, state.server.URL+"/search")

	state.send(t, client, "/search", "/warm")

	// The 403 sets a tracking cookie and then discards the whole session, including that cookie.
	if response := state.send(t, client, "/blocked", "/warm"); response.Code != 403 || response.Headers.Get(warmupStatusHeader) != "reused" {
		t.Fatalf("blocked request: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}

	if cookies := client.cookies.Cookies(target); len(cookies) != 0 || len(client.warmedHosts) != 0 {
		t.Fatalf("rejected session kept cookies %v and warmed hosts %v", cookies, client.warmedHosts)
	}
}

func TestPlainForbiddenKeepsSession(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	state.send(t, client, "/search", "/warm")

	// Without x-warmup-url a 403 is an ordinary response and must not burn the warmed session.
	if response := state.send(t, client, "/blocked", ""); response.Code != 403 || response.Headers.Get(warmupStatusHeader) != "" {
		t.Fatalf("plain blocked request: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}

	response := state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "reused" || state.warmHits.Load() != 1 {
		t.Fatalf("after plain 403: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}
}

func TestWarmupIsPerTargetHost(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)
	port := mustParseUrl(t, state.server.URL).Port()

	state.send(t, client, "/search", "/warm")

	// localhost reaches the same loopback server, but it is another host with its own cookies.
	request := &ActiveRequest{
		Url:       mustParseUrl(t, "https://localhost:"+port+"/open"),
		Method:    "GET",
		Host:      HostInfo{host: "localhost", supportsHttps: true},
		Context:   context.Background(),
		WarmupUrl: mustParseUrl(t, "https://localhost:"+port+"/warm"),
	}

	response, err := client.makeRequestWithClient(request, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 2 {
		t.Fatalf("other host: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}

	response = state.send(t, client, "/search", "/warm")
	if response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "reused" || state.warmHits.Load() != 2 {
		t.Fatalf("first host after other host: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}
}

func TestSessionsAreIsolatedPerClient(t *testing.T) {
	state := newWarmupServer(t)
	first := newTestProxyClient(t)
	second := newTestProxyClient(t)

	state.send(t, first, "/search", "/warm")

	// Each client is a separate browser: another client's session is never shared.
	if response := state.send(t, second, "/search", "/warm"); response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "fresh" || state.warmHits.Load() != 2 {
		t.Fatalf("second client: code %d, status %q, warm hits %d", response.Code, response.Headers.Get(warmupStatusHeader), state.warmHits.Load())
	}

	first.resetSession()
	if response := state.send(t, second, "/search", "/warm"); response.Code != 200 || response.Headers.Get(warmupStatusHeader) != "reused" {
		t.Fatalf("second client after first reset: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}
}

func TestWarmupConcurrentRequests(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	const requestCount = 16
	requests := make([]*ActiveRequest, requestCount)
	for index := range requests {
		requests[index] = state.request(t, "/search", "/warm")
	}

	// All requests start together; the warm-up lock must let exactly one of them warm the session.
	start := make(chan struct{})
	statuses := make(chan string, requestCount)
	var group sync.WaitGroup
	for _, request := range requests {
		group.Go(func() {
			<-start
			response, err := client.makeRequestWithClient(request, 10*time.Second)
			if err != nil {
				t.Error(err)
				return
			}

			if response.Code != 200 || string(response.Body) != "results" {
				t.Errorf("concurrent request: code %d, body %q", response.Code, response.Body)
			}

			statuses <- response.Headers.Get(warmupStatusHeader)
		})
	}

	close(start)
	group.Wait()
	close(statuses)

	counts := map[string]int{}
	for status := range statuses {
		counts[status]++
	}

	if state.warmHits.Load() != 1 || counts["fresh"] != 1 || counts["reused"] != requestCount-1 || state.searchHits.Load() != requestCount {
		t.Fatalf("warm hits %d, search hits %d, statuses %v", state.warmHits.Load(), state.searchHits.Load(), counts)
	}
}

// Run with -race: resets racing with warm-ups may reject individual requests but never corrupt the session.
func TestWarmupConcurrentReset(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	var group sync.WaitGroup
	for range 4 {
		requests := make([]*ActiveRequest, 5)
		for index := range requests {
			requests[index] = state.request(t, "/search", "/warm")
		}

		group.Go(func() {
			for _, request := range requests {
				response, err := client.makeRequestWithClient(request, 10*time.Second)
				if err != nil {
					t.Error(err)
					return
				}

				status := response.Headers.Get(warmupStatusHeader)
				if (response.Code != 200 && response.Code != 403) || (status != "fresh" && status != "reused") {
					t.Errorf("racing request: code %d, status %q", response.Code, status)
				}
			}
		})
	}

	group.Go(func() {
		for range 50 {
			client.resetSession()
			runtime.Gosched()
		}
	})

	group.Wait()

	response := state.send(t, client, "/search", "/warm")
	if response.Code != 200 || string(response.Body) != "results" {
		t.Fatalf("after racing resets: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
	}
}

// Every supported identity must pass the warm-up flow and present one consistent browser on both hops.
func TestWarmupAcrossProfiles(t *testing.T) {
	for _, profile := range browserProfiles {
		t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
			state := newWarmupServer(t)
			client := newProfileProxyClient(t, profile)
			identity := getFakeHeaders(profile)
			snapshot := client.headers.Clone()

			response := state.send(t, client, "/search", "/warm")
			if response.Code != 200 || string(response.Body) != "results" || response.Headers.Get(warmupStatusHeader) != "fresh" {
				t.Fatalf("warm request: code %d, status %q", response.Code, response.Headers.Get(warmupStatusHeader))
			}

			warm := *state.warmHeaders.Load()
			search := *state.searchHeaders.Load()
			userAgent := headerValue(identity, "user-agent")
			if warm.Get("User-Agent") != userAgent || search.Get("User-Agent") != userAgent {
				t.Fatalf("user agents: warm %q, search %q, want %q", warm.Get("User-Agent"), search.Get("User-Agent"), userAgent)
			}

			if warm.Get("Sec-Ch-Ua") != headerValue(identity, "sec-ch-ua") || search.Get("Sec-Ch-Ua") != headerValue(identity, "sec-ch-ua") {
				t.Fatalf("client hints: warm %q, search %q", warm.Get("Sec-Ch-Ua"), search.Get("Sec-Ch-Ua"))
			}

			if warm.Get("Cookie") != "" || !strings.Contains(strings.Join(search.Values("Cookie"), "; "), "session=ok") {
				t.Fatalf("cookies: warm %q, search %q", warm.Values("Cookie"), search.Values("Cookie"))
			}

			if !reflect.DeepEqual(client.headers, snapshot) {
				t.Fatalf("requests mutated the client identity: %v", client.headers)
			}
		})
	}
}

func TestCompressedResponses(t *testing.T) {
	state := newWarmupServer(t)
	client := newTestProxyClient(t)

	// The proxy hands back plain bytes, so Content-Encoding must not reach the caller.
	response := state.send(t, client, "/encoded", "")
	if response.Code != 200 || string(response.Body) != encodedPayload {
		t.Fatalf("stacked encoding: code %d, body %q", response.Code, response.Body)
	}

	if response.Headers.Get("Content-Encoding") != "" || response.Headers.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("unexpected response headers: %v", response.Headers)
	}

	if response, err := client.makeRequestWithClient(state.request(t, "/corrupt", ""), 10*time.Second); err == nil {
		t.Fatalf("corrupt body must fail, got %+v", response)
	}
}

func TestParseWarmupUrl(t *testing.T) {
	for raw, valid := range map[string]bool{
		"":                                      true,
		"https://www.ebay.com/n/all-categories": true,
		"http://example.com/":                   true,
		"/n/all-categories":                     false,
		"ftp://example.com/":                    false,
		"https://":                              false,
		"https://www.ebay.com/%zz":              false,
		"HTTPS://WWW.EBAY.COM/":                 true,
		"https://www.ebay.com:8443/sch?q=1#top": true,
		"https://www.ebay.com:65535/":           true,
		"https://[::1]:8443/":                   true,
		"http://127.0.0.1/warm":                 true,
		"//www.ebay.com/":                       false,
		"www.ebay.com":                          false,
		"https:www.ebay.com":                    false,
		"mailto:user@ebay.com":                  false,
		"javascript:alert(1)":                   false,
		"https://www.ebay.com:port/":            false,
		"https://exa mple.com/":                 false,
		"\thttps://www.ebay.com/":               false,
		"https://:443/":                         false,
		"https://www.ebay.com:0/":               false,
		"https://www.ebay.com:65536/":           false,
	} {
		parsed, err := parseWarmupUrl(raw)
		if valid != (err == nil) {
			t.Errorf("parseWarmupUrl(%q) error = %v", raw, err)
		}

		if raw == "" && parsed != nil {
			t.Error("empty header must mean no warm-up")
		}

		if raw != "" && err == nil && ((parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "") {
			t.Errorf("parseWarmupUrl(%q) accepted %v", raw, parsed)
		}
	}
}
