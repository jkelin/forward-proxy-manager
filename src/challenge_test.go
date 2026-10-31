package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
)

const cfsolveProfile = "../vendor/cfsolve/profiles/safari-26-ios.json"

// fakeSolver answers every clear with answer (or err) and counts the calls.
type fakeSolver struct {
	calls  atomic.Int32
	answer func(target *url.URL) (*challengeClearance, error)
}

func (solver *fakeSolver) clear(_ context.Context, target *url.URL, _ string) (*challengeClearance, error) {
	solver.calls.Add(1)

	return solver.answer(target)
}

func clearedWith(cookieValue string) func(*url.URL) (*challengeClearance, error) {
	return func(*url.URL) (*challengeClearance, error) {
		return &challengeClearance{
			cookies: []*fhttp.Cookie{{Name: "cf_clearance", Value: cookieValue}},
			page:    &Response{Status: ResponseStatusOk, Code: 200, Body: []byte("solved page"), Headers: http.Header{}},
		}, nil
	}
}

// challengeSite challenges every non-iPhone browser, and the iPhone too unless it sends cf_clearance=valid
// (or passIphone is set). Setting clearanceValid to false expires every issued clearance.
type challengeSite struct {
	passIphone     atomic.Bool
	clearanceValid atomic.Bool
}

func (site *challengeSite) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	iphone := strings.Contains(request.UserAgent(), "iPhone")
	cookie, _ := request.Cookie("cf_clearance")
	cleared := cookie != nil && cookie.Value == "valid" && site.clearanceValid.Load()

	if iphone && (site.passIphone.Load() || cleared) {
		_, _ = writer.Write([]byte("protected page"))

		return
	}

	writer.Header().Set(cfMitigatedHeader, "challenge")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = writer.Write([]byte("Just a moment..."))
}

func newChallengeClient(t *testing.T, solver challengeSolver) *ProxyClient {
	t.Helper()

	identity, err := loadSolverIdentity(cfsolveProfile)
	if err != nil {
		t.Fatal(err)
	}

	client := newProfileProxyClient(t, profiles.Chrome_144)
	if err := client.enableChallenges(&challengeConfig{solver: solver, identity: identity}, "socks5h://unused:1"); err != nil {
		t.Fatal(err)
	}

	return client
}

func sendChallenged(t *testing.T, client *ProxyClient, target string) *Response {
	t.Helper()

	request := &ActiveRequest{
		Url:     mustParseUrl(t, target),
		Method:  "GET",
		Host:    HostInfo{host: "127.0.0.1"},
		Context: context.Background(),
	}

	response, err := client.makeRequestWithClient(request, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return response
}

func expectAnswer(t *testing.T, response *Response, code int, body string, status string) {
	t.Helper()

	if response.Code != code || string(response.Body) != body || response.Headers.Get(challengeStatusHeader) != status {
		t.Fatalf("got %d %q %s=%q, want %d %q %q", response.Code, response.Body, challengeStatusHeader, response.Headers.Get(challengeStatusHeader), code, body, status)
	}
}

func TestChallengeSolvedThenReused(t *testing.T) {
	site := &challengeSite{}
	site.clearanceValid.Store(true)
	server := startTargetServer(t, site)
	solver := &fakeSolver{answer: clearedWith("valid")}
	client := newChallengeClient(t, solver)

	expectAnswer(t, sendChallenged(t, client, server.URL+"/a"), 200, "solved page", challengeSolved)
	expectAnswer(t, sendChallenged(t, client, server.URL+"/b"), 200, "protected page", challengeReused)

	if solver.calls.Load() != 1 {
		t.Fatalf("solver calls %d, want 1", solver.calls.Load())
	}
}

func TestChallengeOnlyForBrowserIdentity(t *testing.T) {
	site := &challengeSite{}
	site.passIphone.Store(true)
	server := startTargetServer(t, site)
	solver := &fakeSolver{answer: clearedWith("valid")}
	client := newChallengeClient(t, solver)

	expectAnswer(t, sendChallenged(t, client, server.URL+"/a"), 200, "protected page", challengeUnchallenged)
	expectAnswer(t, sendChallenged(t, client, server.URL+"/b"), 200, "protected page", challengeReused)

	if solver.calls.Load() != 0 {
		t.Fatalf("solver calls %d, want 0", solver.calls.Load())
	}
}

func TestChallengeExpiredClearanceSolvesAgain(t *testing.T) {
	site := &challengeSite{}
	site.clearanceValid.Store(true)
	server := startTargetServer(t, site)
	solver := &fakeSolver{answer: clearedWith("valid")}
	client := newChallengeClient(t, solver)

	expectAnswer(t, sendChallenged(t, client, server.URL+"/a"), 200, "solved page", challengeSolved)

	site.clearanceValid.Store(false)
	solver.answer = func(*url.URL) (*challengeClearance, error) {
		site.clearanceValid.Store(true)

		return clearedWith("valid")(nil)
	}

	expectAnswer(t, sendChallenged(t, client, server.URL+"/b"), 200, "solved page", challengeSolved)
	expectAnswer(t, sendChallenged(t, client, server.URL+"/c"), 200, "protected page", challengeReused)

	if solver.calls.Load() != 2 {
		t.Fatalf("solver calls %d, want 2", solver.calls.Load())
	}
}

func TestChallengeFailedSolveKeepsChallenge(t *testing.T) {
	site := &challengeSite{}
	server := startTargetServer(t, site)
	solver := &fakeSolver{answer: func(*url.URL) (*challengeClearance, error) {
		return nil, &solverFailure{kind: "rejected", message: "no"}
	}}
	client := newChallengeClient(t, solver)

	expectAnswer(t, sendChallenged(t, client, server.URL+"/a"), 403, "Just a moment...", challengeFailed)

	if client.challenges.serves("127.0.0.1") {
		t.Fatal("a failed solve must not serve the host with the solver identity")
	}

	expectAnswer(t, sendChallenged(t, client, server.URL+"/b"), 403, "Just a moment...", challengeFailed)

	if solver.calls.Load() != 2 {
		t.Fatalf("solver calls %d, want 2", solver.calls.Load())
	}
}

func TestChallengeIgnoresPlainBlocks(t *testing.T) {
	server := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
	}))
	solver := &fakeSolver{answer: clearedWith("valid")}
	client := newChallengeClient(t, solver)

	expectAnswer(t, sendChallenged(t, client, server.URL+"/a"), 403, "", "")

	if solver.calls.Load() != 0 {
		t.Fatalf("solver calls %d, want 0", solver.calls.Load())
	}
}

func TestIsChallenge(t *testing.T) {
	challengeHeaders := http.Header{cfMitigatedHeader: {"challenge"}}

	cases := []struct {
		name     string
		response Response
		want     bool
	}{
		{"403 challenge", Response{Status: ResponseStatusOk, Code: 403, Headers: challengeHeaders}, true},
		{"503 challenge", Response{Status: ResponseStatusOk, Code: 503, Headers: challengeHeaders}, true},
		{"403 without marker", Response{Status: ResponseStatusOk, Code: 403, Headers: http.Header{}}, false},
		{"200 with marker", Response{Status: ResponseStatusOk, Code: 200, Headers: challengeHeaders}, false},
		{"timeout", Response{Status: ResponseStatusTimeout, Code: 403, Headers: challengeHeaders}, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isChallenge(&testCase.response); got != testCase.want {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestParseSolverIdentity(t *testing.T) {
	identity, err := loadSolverIdentity(cfsolveProfile)
	if err != nil {
		t.Fatal(err)
	}

	if identity.tlsProfile.GetClientHelloStr() != profiles.Safari_IOS_26_0.GetClientHelloStr() {
		t.Fatalf("tls profile %s", identity.tlsProfile.GetClientHelloStr())
	}

	order := identity.headers[fhttp.HeaderOrderKey]
	if len(order) != 9 || order[1] != "user-agent" || !strings.Contains(identity.headers["user-agent"][0], "iPhone") {
		t.Fatalf("headers %v", identity.headers)
	}

	invalid := []struct {
		name string
		raw  string
	}{
		{"not json", `{`},
		{"unknown transport profile", `{"transport":{"profile":"chrome_200.1","headers":{"User-Agent":"x"},"requests":{"headers":{"navigation":["user-agent"]}}}}`},
		{"no user agent", `{"transport":{"profile":"safari_ios_26.2","headers":{},"requests":{"headers":{"navigation":["user-agent"]}}}}`},
		{"no navigation", `{"transport":{"profile":"safari_ios_26.2","headers":{"User-Agent":"x"},"requests":{"headers":{"navigation":[]}}}}`},
	}

	for _, testCase := range invalid {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseSolverIdentity("test.json", []byte(testCase.raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseSolverAnswer(t *testing.T) {
	clearance, err := parseSolverAnswer([]byte(`{"outcome":"cleared","cookies":[{"name":"cf_clearance","value":"v","domain":".example.com","path":"/","secure":true,"httpOnly":true,"expires":1900000000}],"page":{"url":"https://example.com/","status":200,"html":"<p>ok</p>"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	cookie := clearance.cookies[0]
	if cookie.Name != "cf_clearance" || cookie.Domain != ".example.com" || !cookie.HttpOnly || cookie.Expires.Unix() != 1900000000 {
		t.Fatalf("cookie %+v", cookie)
	}

	if clearance.page.Code != 200 || string(clearance.page.Body) != "<p>ok</p>" {
		t.Fatalf("page %+v", clearance.page)
	}

	_, err = parseSolverAnswer([]byte(`{"outcome":"failed","kind":"rejected","message":"no"}`))
	var failure *solverFailure
	if !errors.As(err, &failure) || failure.kind != "rejected" {
		t.Fatalf("got %v, want a rejected solverFailure", err)
	}

	invalid := []struct {
		name string
		raw  string
	}{
		{"empty", ``},
		{"unknown outcome", `{"outcome":"maybe"}`},
		{"cleared without page", `{"outcome":"cleared","cookies":[]}`},
		{"invalid status", `{"outcome":"cleared","cookies":[],"page":{"url":"u","status":0,"html":""}}`},
		{"nameless cookie", `{"outcome":"cleared","cookies":[{"name":"","value":"v"}],"page":{"url":"u","status":200,"html":""}}`},
	}

	for _, testCase := range invalid {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseSolverAnswer([]byte(testCase.raw))
			if err == nil || errors.As(err, &failure) {
				t.Fatalf("got %v, want an invalid-answer error", err)
			}
		})
	}
}
