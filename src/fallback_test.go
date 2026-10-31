package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const defaultBlockBody = `(?i)<title>(pardon our interruption|just a moment|access denied|attention required)|serviceName\W+splashui`

func testFallbackPolicy(t *testing.T, afterBlocks int, attempts int, retries int) *fallbackPolicy {
	t.Helper()

	policy, err := newFallbackPolicy(afterBlocks, attempts, []int{403, 429}, defaultBlockBody, retries)
	if err != nil {
		t.Fatal(err)
	}

	return policy
}

func TestBlockBodyPatternDefaultIsTheTestedOne(t *testing.T) {
	field, _ := reflect.TypeFor[GlobalConfiguration]().FieldByName("BlockBodyPattern")
	if tagDefault := field.Tag.Get("default"); tagDefault != defaultBlockBody {
		t.Fatalf("BLOCK_BODY_PATTERN default is %q, tests use %q", tagDefault, defaultBlockBody)
	}
}

func TestNewFallbackPolicyRejectsInvalidSettings(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		afterBlocks int
		attempts    int
		codes       []int
		body        string
		retries     int
	}{
		{"no blocks", 0, 1, []int{403}, "", 1},
		{"unreachable threshold", 3, 1, []int{403}, "", 1},
		{"no fallback attempts", 1, 0, []int{403}, "", 1},
		{"not a status code", 1, 1, []int{4030}, "", 1},
		{"invalid pattern", 1, 1, nil, "(", 1},
		{"nothing counts as blocked", 1, 1, nil, "", 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := newFallbackPolicy(testCase.afterBlocks, testCase.attempts, testCase.codes, testCase.body, testCase.retries); err == nil {
				t.Fatal("expected an error")
			}
		})
	}

	if _, err := newFallbackPolicy(2, 1, nil, defaultBlockBody, 1); err != nil {
		t.Fatalf("threshold RETRIES+1 with a pattern only must be valid: %v", err)
	}
}

func TestFallbackPolicyIsBlocked(t *testing.T) {
	policy := testFallbackPolicy(t, 1, 1, 0)
	page := func(code int, body string) *Response {
		return &Response{Status: ResponseStatusOk, Code: code, Body: []byte(body), Headers: http.Header{}}
	}
	late := strings.Repeat(" ", blockBodyScanLimit) + "<title>Access Denied</title>"

	for _, testCase := range []struct {
		name     string
		response *Response
		blocked  bool
	}{
		{"403", page(403, ""), true},
		{"429", page(429, ""), true},
		{"eBay interstitial with 200", page(200, "<html><head><title>Pardon Our Interruption...</title>"), true},
		{"localized eBay interstitial", page(200, `<title>Sorry voor de onderbreking...</title><script data-inlinepayload={"loggerProps":{"serviceName":"splashui"}}>`), true},
		{"Cloudflare challenge title", page(503, "<title>Just a moment...</title>"), true},
		{"normal page", page(200, "<title>Xeon | eBay</title>"), false},
		{"404", page(404, "<title>Not found</title>"), false},
		{"marker past the scan limit", page(200, late), false},
		{"proxy failure", &Response{Status: ResponseStatusProxyUnreachable}, false},
		{"timeout", &Response{Status: ResponseStatusTimeout}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if blocked := policy.isBlocked(testCase.response); blocked != testCase.blocked {
				t.Fatalf("got %v, want %v", blocked, testCase.blocked)
			}
		})
	}
}

func blockedPage(code int, body string) *Response {
	return &Response{Status: ResponseStatusOk, Code: code, Body: []byte(body), Headers: http.Header{}}
}

func TestBlockedRequestsMoveToTheFallbackPool(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		blocked *Response
	}{
		{"403", blockedPage(403, "")},
		{"interstitial with 200", blockedPage(200, "<title>Pardon Our Interruption...</title>")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(1)
			scheduler.fallback = testFallbackPolicy(t, 2, 2, 1)
			primary := newFakeExit("primary", respondSequence(testCase.blocked))
			fallback := newFakeExit("fallback", respondWith(200))
			startScheduler(t, scheduler, primary)
			scheduler.setFallbackProxies([]proxyExit{fallback})

			response := execute(t, scheduler, "https://a.test/", nil, nil)

			if response.Code != 200 || response.Headers.Get(poolHeader) != "fallback" {
				t.Fatalf("got %d from the %q pool", response.Code, response.Headers.Get(poolHeader))
			}

			if primary.attemptCount() != 2 || fallback.attemptCount() != 1 {
				t.Fatalf("primary %d, fallback %d attempts; want 2 and 1", primary.attemptCount(), fallback.attemptCount())
			}
		})
	}
}

func TestFallbackAttemptsAreBounded(t *testing.T) {
	scheduler := newTestScheduler(1)
	scheduler.fallback = testFallbackPolicy(t, 1, 2, 1)
	primary := newFakeExit("primary", respondWith(403))
	fallback := newFakeExit("fallback", respondWith(429))
	startScheduler(t, scheduler, primary)
	scheduler.setFallbackProxies([]proxyExit{fallback})

	response := execute(t, scheduler, "https://a.test/", nil, nil)

	if response.Code != 429 || response.Headers.Get(poolHeader) != "fallback" {
		t.Fatalf("got %d from the %q pool", response.Code, response.Headers.Get(poolHeader))
	}

	// Fallback blocks use their own budget, not RETRIES: one primary attempt, then two fallback ones.
	if primary.attemptCount() != 1 || fallback.attemptCount() != 2 {
		t.Fatalf("primary %d, fallback %d attempts; want 1 and 2", primary.attemptCount(), fallback.attemptCount())
	}
}

func TestBlockedRequestsStayPrimaryWithoutFallbackExits(t *testing.T) {
	scheduler := newTestScheduler(1)
	scheduler.fallback = testFallbackPolicy(t, 2, 2, 1)
	primary := newFakeExit("primary", respondWith(403))
	startScheduler(t, scheduler, primary)

	response := execute(t, scheduler, "https://a.test/", nil, nil)

	if response.Code != 403 || response.Headers.Get(poolHeader) != "primary" {
		t.Fatalf("got %d from the %q pool", response.Code, response.Headers.Get(poolHeader))
	}

	if primary.attemptCount() != 2 {
		t.Fatalf("got %d primary attempts, want RETRIES+1 = 2", primary.attemptCount())
	}
}

func TestUnblockedAnswersNeverUseTheFallbackPool(t *testing.T) {
	scheduler := newTestScheduler(1)
	scheduler.fallback = testFallbackPolicy(t, 1, 1, 1)
	primary := newFakeExit("primary", respondWith(404))
	fallback := newFakeExit("fallback", respondWith(200))
	startScheduler(t, scheduler, primary)
	scheduler.setFallbackProxies([]proxyExit{fallback})

	response := execute(t, scheduler, "https://a.test/", nil, nil)

	if response.Code != 404 || primary.attemptCount() != 1 || fallback.attemptCount() != 0 {
		t.Fatalf("got %d after %d primary and %d fallback attempts", response.Code, primary.attemptCount(), fallback.attemptCount())
	}
}

func TestWithoutFallbackPolicyBlocksFinishAsBefore(t *testing.T) {
	scheduler := newTestScheduler(1)
	primary := newFakeExit("primary", respondSequence(blockedPage(200, "<title>Pardon Our Interruption...</title>")))
	startScheduler(t, scheduler, primary)

	response := execute(t, scheduler, "https://a.test/", nil, nil)

	if primary.attemptCount() != 1 || response.Headers.Get(poolHeader) != "" {
		t.Fatalf("got %d attempts and pool header %q", primary.attemptCount(), response.Headers.Get(poolHeader))
	}
}
