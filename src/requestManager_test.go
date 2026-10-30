package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/btree"
	"github.com/throttled/throttled/v2"
)

// fakeExit is a scriptable proxyExit.
type fakeExit struct {
	name string
	// respond answers attempt number attempt (0-based, per exit) of request.
	respond func(request *ActiveRequest, attempt int) (*Response, error)

	lock        sync.Mutex
	limited     bool
	retryAfter  time.Duration
	tokens      int // remaining unlimited RateLimit calls; negative means unlimited
	rateChecks  []string
	attempts    []*ActiveRequest
	priorities  []int64
	unreachable int
	backOffs    []string
}

func newFakeExit(name string, respond func(request *ActiveRequest, attempt int) (*Response, error)) *fakeExit {
	return &fakeExit{name: name, respond: respond, tokens: -1}
}

func (exit *fakeExit) RateLimit(host HostInfo) (bool, throttled.RateLimitResult, error) {
	exit.lock.Lock()
	defer exit.lock.Unlock()

	exit.rateChecks = append(exit.rateChecks, host.host)
	if exit.limited || exit.tokens == 0 {
		return true, throttled.RateLimitResult{RetryAfter: exit.retryAfter}, nil
	}

	if exit.tokens > 0 {
		exit.tokens--
	}

	return false, throttled.RateLimitResult{}, nil
}

func (exit *fakeExit) backOff(host HostInfo) {
	exit.lock.Lock()
	defer exit.lock.Unlock()

	exit.backOffs = append(exit.backOffs, host.host)
}

func (exit *fakeExit) markUnreachable() {
	exit.lock.Lock()
	defer exit.lock.Unlock()

	exit.unreachable++
	exit.limited = true
}

func (exit *fakeExit) makeRequestWithClient(request *ActiveRequest, timeout time.Duration) (*Response, error) {
	exit.lock.Lock()
	attempt := len(exit.attempts)
	exit.attempts = append(exit.attempts, request)
	exit.priorities = append(exit.priorities, request.Priority)
	exit.lock.Unlock()

	return exit.respond(request, attempt)
}

func (exit *fakeExit) attemptCount() int {
	exit.lock.Lock()
	defer exit.lock.Unlock()

	return len(exit.attempts)
}

// attempt returns the request passed to attempt number index.
func (exit *fakeExit) attempt(index int) *ActiveRequest {
	exit.lock.Lock()
	defer exit.lock.Unlock()

	return exit.attempts[index]
}

func respondWith(code int) func(*ActiveRequest, int) (*Response, error) {
	return func(*ActiveRequest, int) (*Response, error) {
		return &Response{Status: ResponseStatusOk, Code: code, Headers: http.Header{}}, nil
	}
}

// respondSequence answers attempt i with responses[i], repeating the last one.
func respondSequence(responses ...*Response) func(*ActiveRequest, int) (*Response, error) {
	return func(_ *ActiveRequest, attempt int) (*Response, error) {
		response := *responses[min(attempt, len(responses)-1)]

		return &response, nil
	}
}

// waitForContext blocks like a hung target until the attempt's caller goes away.
func waitForContext(request *ActiveRequest, _ int) (*Response, error) {
	<-request.Context.Done()

	return nil, request.Context.Err()
}

func identityOrder(length int) []int {
	indices := make([]int, length)
	for index := range indices {
		indices[index] = index
	}

	return indices
}

func newTestScheduler(retries int) *RequestScheduler {
	scheduler := newRequestScheduler(retries, time.Second)
	scheduler.shuffle = identityOrder
	scheduler.idleInterval = 10 * time.Millisecond

	return scheduler
}

func execute(t *testing.T, scheduler *RequestScheduler, rawUrl string, retryOnCodes []uint16, warmupUrl *url.URL) *Response {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	response, err := scheduler.execute(ctx, mustParseUrl(t, rawUrl), 0, retryOnCodes, warmupUrl)
	if err != nil {
		t.Fatal(err)
	}

	if ctx.Err() != nil {
		t.Fatal("request did not finish in time")
	}

	return response
}

func TestClassifyAttempt(t *testing.T) {
	plain := &ActiveRequest{Url: &url.URL{Host: "a.test"}}
	warm := &ActiveRequest{Url: &url.URL{Host: "a.test"}, WarmupUrl: &url.URL{Host: "a.test"}}
	withCodes := &ActiveRequest{Url: &url.URL{Host: "a.test"}, RetryOnCodes: []uint16{503, 418}}

	ok := func(code int) *Response { return &Response{Status: ResponseStatusOk, Code: code} }
	status := func(status ResponseStatus) *Response { return &Response{Status: status} }

	for _, testCase := range []struct {
		name           string
		request        *ActiveRequest
		response       *Response
		err            error
		decision       attemptDecision
		expectedStatus ResponseStatus
		sameResponse   bool
	}{
		{"200", plain, ok(200), nil, decisionFinish, ResponseStatusOk, true},
		{"404", plain, ok(404), nil, decisionFinish, ResponseStatusOk, true},
		{"502", plain, ok(502), nil, decisionRetry, ResponseStatusOk, true},
		{"missing code", plain, ok(0), nil, decisionRetry, ResponseStatusOk, true},
		{"429", plain, ok(429), nil, decisionRetry, ResponseStatusOk, true},
		{"403 without warm-up", plain, ok(403), nil, decisionFinish, ResponseStatusOk, true},
		{"403 with warm-up", warm, ok(403), nil, decisionRetry, ResponseStatusOk, true},
		{"retry code", withCodes, ok(503), nil, decisionRetry, ResponseStatusOk, true},
		{"other retry code", withCodes, ok(418), nil, decisionRetry, ResponseStatusOk, true},
		{"code not listed", withCodes, ok(500), nil, decisionFinish, ResponseStatusOk, true},
		{"503 without retry codes", plain, ok(503), nil, decisionFinish, ResponseStatusOk, true},
		{"timeout", plain, status(ResponseStatusTimeout), nil, decisionRetry, ResponseStatusTimeout, true},
		{"host unreachable", plain, status(ResponseStatusHostUnreachable), nil, decisionRetry, ResponseStatusHostUnreachable, true},
		{"proxy unreachable", plain, status(ResponseStatusProxyUnreachable), nil, decisionRequeue, ResponseStatusProxyUnreachable, true},
		{"unknown status", plain, status(ResponseStatusUnknownError), nil, decisionFinish, ResponseStatusUnknownError, true},
		{"cancelled", plain, nil, context.Canceled, decisionFinish, ResponseStatusRequestCancelled, false},
		{"caller deadline", plain, nil, fmt.Errorf("get: %w", context.DeadlineExceeded), decisionFinish, ResponseStatusRequestCancelled, false},
		{"eof", plain, nil, &url.Error{Op: "Get", URL: "x", Err: io.EOF}, decisionRetry, ResponseStatusUnknownError, false},
		{"unexpected eof", plain, nil, io.ErrUnexpectedEOF, decisionRetry, ResponseStatusUnknownError, false},
		{"other error", plain, nil, errors.New("boom"), decisionFinish, ResponseStatusUnknownError, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decision, final := classifyAttempt(testCase.request, testCase.response, testCase.err)
			if decision != testCase.decision || final == nil || final.Status != testCase.expectedStatus {
				t.Fatalf("got (%d, %+v), want (%d, status %d)", decision, final, testCase.decision, testCase.expectedStatus)
			}

			if testCase.sameResponse && final != testCase.response {
				t.Fatal("the attempt's own response must be handed on")
			}
		})
	}
}

func TestRequestOrder(t *testing.T) {
	for _, testCase := range []struct {
		first    *ActiveRequest
		second   *ActiveRequest
		expected bool
	}{
		{&ActiveRequest{Priority: 5, Id: 9}, &ActiveRequest{Priority: 1, Id: 1}, true},
		{&ActiveRequest{Priority: 1, Id: 1}, &ActiveRequest{Priority: 5, Id: 9}, false},
		{&ActiveRequest{Priority: 0, Id: 1}, &ActiveRequest{Priority: 0, Id: 2}, true},
		{&ActiveRequest{Priority: 0, Id: 2}, &ActiveRequest{Priority: 0, Id: 1}, false},
		{&ActiveRequest{Priority: -1, Id: 1}, &ActiveRequest{Priority: 0, Id: 2}, false},
		{&ActiveRequest{Priority: 3, Id: 3}, &ActiveRequest{Priority: 3, Id: 3}, false},
	} {
		if actual := requestOrder(testCase.first, testCase.second); actual != testCase.expected {
			t.Errorf("requestOrder(p%d#%d, p%d#%d) = %v", testCase.first.Priority, testCase.first.Id, testCase.second.Priority, testCase.second.Id, actual)
		}
	}
}

func TestSubmit(t *testing.T) {
	scheduler := newTestScheduler(0)
	published := make(chan interface{}, 10)
	scheduler.requests.Register(published)

	warmup := mustParseUrl(t, "https://a.test/warm")
	first, callback, err := scheduler.submit(context.Background(), mustParseUrl(t, "https://a.test/x"), 7, []uint16{503}, warmup)
	if err != nil || callback == nil {
		t.Fatal(err)
	}

	second, _, err := scheduler.submit(context.Background(), mustParseUrl(t, "https://b.test/y"), 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if first.Id != 0 || second.Id != 1 {
		t.Fatalf("ids must count from 0, got %d %d", first.Id, second.Id)
	}

	if first.Priority != 7 || first.Method != http.MethodGet || first.Status != RequestStatusPending ||
		first.WarmupUrl != warmup || !slices.Equal(first.RetryOnCodes, []uint16{503}) || first.Host.host != "a.test" {
		t.Fatalf("unexpected request %+v", first)
	}

	for _, expected := range []*ActiveRequest{first, second} {
		select {
		case actual := <-published:
			if actual != expected {
				t.Fatal("requests must be published in order")
			}
		case <-time.After(time.Second):
			t.Fatal("request not published")
		}
	}
}

// FPM never resolves or probes a target itself: an unresolvable host still goes to an exit, so only exits decide
// reachability and the FPM machine's IP never reaches a target.
func TestSubmitSendsUnresolvableHostsToExits(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondWith(http.StatusOK))
	startScheduler(t, scheduler, exit)

	response, err := scheduler.execute(context.Background(), mustParseUrl(t, "https://down.invalid/"), 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if response.Status != ResponseStatusOk || response.Code != http.StatusOK || exit.attemptCount() != 1 {
		t.Fatalf("got %+v after %d attempt(s)", response, exit.attemptCount())
	}
}

func TestSubmitRejectsInvalidTargets(t *testing.T) {
	scheduler := newTestScheduler(0)
	target := &url.URL{Scheme: "ftp", Host: "a.test"}

	if _, _, err := scheduler.submit(context.Background(), target, 0, nil, nil); !errors.Is(err, errInvalidTarget) {
		t.Fatalf("got %v", err)
	}

	if _, err := scheduler.execute(context.Background(), target, 0, nil, nil); !errors.Is(err, errInvalidTarget) {
		t.Fatalf("got %v", err)
	}
}

func TestSubmitIdsAreUniqueUnderConcurrency(t *testing.T) {
	scheduler := newTestScheduler(0)

	var lock sync.Mutex
	seen := make(map[uint64]bool)

	target := mustParseUrl(t, "https://a.test/")

	var group sync.WaitGroup
	for range 64 {
		group.Go(func() {
			request, _, err := scheduler.submit(context.Background(), target, 0, nil, nil)
			if err != nil {
				t.Error(err)
				return
			}

			lock.Lock()
			defer lock.Unlock()

			if seen[request.Id] {
				t.Errorf("duplicate id %d", request.Id)
			}

			seen[request.Id] = true
		})
	}

	group.Wait()
}

func queuedRequest(id uint64, priority int64, ctx context.Context) *ActiveRequest {
	return &ActiveRequest{
		Id:       id,
		Priority: priority,
		Url:      &url.URL{Scheme: "https", Host: fmt.Sprintf("r%d.test", id)},
		Host:     HostInfo{host: fmt.Sprintf("r%d.test", id)},
		Context:  ctx,
		Callback: make(chan *Response, 1),
	}
}

func TestDispatchPriorityOrder(t *testing.T) {
	scheduler := newTestScheduler(0)
	queue := btree.NewG(32, requestOrder)

	for _, request := range []*ActiveRequest{
		queuedRequest(0, 0, context.Background()),
		queuedRequest(1, 10, context.Background()),
		queuedRequest(2, -5, context.Background()),
		queuedRequest(3, 10, context.Background()),
		queuedRequest(4, 3, context.Background()),
	} {
		queue.ReplaceOrInsert(request)
	}

	started := make(chan *ActiveRequest, 5)
	exit := newFakeExit("only", func(request *ActiveRequest, _ int) (*Response, error) {
		started <- request
		return &Response{Status: ResponseStatusOk, Code: 200}, nil
	})
	exit.tokens = 3
	exit.retryAfter = 4 * time.Millisecond

	wait := scheduler.dispatch(queue, []proxyExit{exit})

	// Highest priority first, ties by age; the exit has 3 tokens, so the last two stay queued.
	expectedChecks := []string{"r1.test", "r3.test", "r4.test", "r0.test", "r2.test"}
	if !slices.Equal(exit.rateChecks, expectedChecks) {
		t.Fatalf("checked %v, want %v", exit.rateChecks, expectedChecks)
	}

	if queue.Len() != 2 || queue.Has(queuedRequest(1, 10, nil)) {
		t.Fatalf("expected 2 queued requests, got %d", queue.Len())
	}

	if wait != 4*time.Millisecond {
		t.Fatalf("wait must follow the rate-limit hint, got %v", wait)
	}

	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("dispatched request never ran")
		}
	}
}

func TestDispatchWait(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		proxies    int
		retryAfter time.Duration
		queued     bool
		expected   time.Duration
	}{
		{"empty queue", 1, 0, false, 10 * time.Millisecond},
		{"no proxies", 0, 0, true, 10 * time.Millisecond},
		{"short hint", 2, 3 * time.Millisecond, true, 3 * time.Millisecond},
		{"hint longer than idle", 1, time.Hour, true, 10 * time.Millisecond},
		{"negative hint", 1, -1, true, 10 * time.Millisecond},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(0)
			queue := btree.NewG(32, requestOrder)
			if testCase.queued {
				queue.ReplaceOrInsert(queuedRequest(0, 0, context.Background()))
			}

			proxies := make([]proxyExit, 0)
			for index := range testCase.proxies {
				exit := newFakeExit(fmt.Sprint(index), respondWith(200))
				exit.limited = true
				exit.retryAfter = testCase.retryAfter
				proxies = append(proxies, exit)
			}

			if wait := scheduler.dispatch(queue, proxies); wait != testCase.expected {
				t.Fatalf("got %v, want %v", wait, testCase.expected)
			}

			if testCase.queued && queue.Len() != 1 {
				t.Fatal("limited request must stay queued")
			}
		})
	}
}

func TestDispatchDropsCancelled(t *testing.T) {
	scheduler := newTestScheduler(0)
	finished := make(chan interface{}, 1)
	scheduler.finished.Register(finished)

	queue := btree.NewG(32, requestOrder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	callback := make(chan *Response, 1)
	cancelled := queuedRequest(0, 100, ctx)
	cancelled.Callback = callback
	live := queuedRequest(1, 0, context.Background())
	queue.ReplaceOrInsert(cancelled)
	queue.ReplaceOrInsert(live)

	exit := newFakeExit("only", respondWith(200))
	exit.limited = true
	scheduler.dispatch(queue, []proxyExit{exit})

	if queue.Len() != 1 || !queue.Has(live) {
		t.Fatal("only the cancelled request may be removed")
	}

	if !slices.Equal(exit.rateChecks, []string{"r1.test"}) {
		t.Fatalf("cancelled requests must not use exits, checks %v", exit.rateChecks)
	}

	response := <-callback
	if response.Status != ResponseStatusRequestCancelled || cancelled.Status != RequestStatusFinished {
		t.Fatalf("got %+v, status %d", response, cancelled.Status)
	}

	select {
	case request := <-finished:
		if request != cancelled {
			t.Fatal("wrong finished request")
		}
	case <-time.After(time.Second):
		t.Fatal("dropped request must be reported as finished")
	}
}

func TestDispatchShufflesExits(t *testing.T) {
	scheduler := newTestScheduler(0)
	scheduler.shuffle = func(length int) []int {
		indices := identityOrder(length)
		slices.Reverse(indices)

		return indices
	}

	first := newFakeExit("first", respondWith(200))
	second := newFakeExit("second", respondWith(200))
	third := newFakeExit("third", respondWith(200))
	third.limited = true

	queue := btree.NewG(32, requestOrder)
	queue.ReplaceOrInsert(queuedRequest(0, 0, context.Background()))
	scheduler.dispatch(queue, []proxyExit{first, second, third})

	if len(third.rateChecks) != 1 || len(second.rateChecks) != 1 || len(first.rateChecks) != 0 {
		t.Fatalf("exits must be tried in shuffled order until one is free: %v %v %v", first.rateChecks, second.rateChecks, third.rateChecks)
	}
}

func TestDefaultShuffleIsPermutation(t *testing.T) {
	scheduler := newRequestScheduler(0, time.Second)

	for length := range 8 {
		indices := scheduler.shuffle(length)
		slices.Sort(indices)

		if !slices.Equal(indices, identityOrder(length)) {
			t.Fatalf("shuffle(%d) is not a permutation: %v", length, indices)
		}
	}
}

func TestSchedulerRetries(t *testing.T) {
	ok := func(code int) *Response {
		return &Response{Status: ResponseStatusOk, Code: code, Headers: http.Header{}}
	}

	for _, testCase := range []struct {
		name         string
		retries      int
		retryOnCodes []uint16
		warmup       bool
		responses    []*Response
		attempts     int
		expected     *Response
	}{
		{"success", 2, nil, false, []*Response{ok(200)}, 1, ok(200)},
		{"502 then success", 2, nil, false, []*Response{ok(502), ok(200)}, 2, ok(200)},
		{"502 exhausts retries", 2, nil, false, []*Response{ok(502)}, 3, ok(502)},
		{"no retries", 0, nil, false, []*Response{ok(502), ok(200)}, 1, ok(502)},
		{"missing code retries", 1, nil, false, []*Response{ok(0), ok(204)}, 2, ok(204)},
		{"retry code", 1, []uint16{503}, false, []*Response{ok(503), ok(200)}, 2, ok(200)},
		{"unlisted code", 1, []uint16{503}, false, []*Response{ok(500), ok(200)}, 1, ok(500)},
		{"rejected session", 1, nil, true, []*Response{ok(403), ok(200)}, 2, ok(200)},
		{"403 without session", 1, nil, false, []*Response{ok(403), ok(200)}, 1, ok(403)},
		{"timeout", 1, nil, false, []*Response{{Status: ResponseStatusTimeout}, ok(200)}, 2, ok(200)},
		{"timeout exhausts", 1, nil, false, []*Response{{Status: ResponseStatusTimeout}}, 2, &Response{Status: ResponseStatusTimeout}},
		{"host unreachable exhausts", 0, nil, false, []*Response{{Status: ResponseStatusHostUnreachable}}, 1, &Response{Status: ResponseStatusHostUnreachable}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(testCase.retries)
			exit := newFakeExit("only", respondSequence(testCase.responses...))
			startScheduler(t, scheduler, exit)

			var warmup *url.URL
			if testCase.warmup {
				warmup = mustParseUrl(t, "https://a.test/warm")
			}

			response := execute(t, scheduler, "https://a.test/page", testCase.retryOnCodes, warmup)
			if response.Status != testCase.expected.Status || response.Code != testCase.expected.Code {
				t.Fatalf("got %+v, want %+v", response, testCase.expected)
			}

			if exit.attemptCount() != testCase.attempts {
				t.Fatalf("got %d attempts, want %d", exit.attemptCount(), testCase.attempts)
			}

			request := exit.attempts[0]
			request.Lock.Lock()
			defer request.Lock.Unlock()

			if request.Status != RequestStatusFinished || int(request.Retries) != testCase.attempts-1 {
				t.Fatalf("request ended %d with %d retries", request.Status, request.Retries)
			}
		})
	}
}

func TestSchedulerErrors(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		errors   []error
		attempts int
	}{
		{"unknown error finishes", []error{errors.New("tls: bad certificate")}, 1},
		{"eof retries then fails", []error{io.EOF, io.EOF}, 2},
		{"eof then unknown", []error{io.ErrUnexpectedEOF, errors.New("boom")}, 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(1)
			exit := newFakeExit("only", func(_ *ActiveRequest, attempt int) (*Response, error) {
				return nil, testCase.errors[min(attempt, len(testCase.errors)-1)]
			})
			startScheduler(t, scheduler, exit)

			response := execute(t, scheduler, "https://a.test/", nil, nil)
			if response.Status != ResponseStatusUnknownError || exit.attemptCount() != testCase.attempts {
				t.Fatalf("got %+v after %d attempts", response, exit.attemptCount())
			}
		})
	}
}

func TestSchedulerProxyUnreachableMovesOn(t *testing.T) {
	scheduler := newTestScheduler(0)
	broken := newFakeExit("broken", respondSequence(&Response{Status: ResponseStatusProxyUnreachable}))
	healthy := newFakeExit("healthy", respondWith(200))
	startScheduler(t, scheduler, broken, healthy)

	// Retries are 0, yet the request survives: a broken exit does not use up the request's retries.
	response := execute(t, scheduler, "https://a.test/", nil, nil)
	if response.Code != 200 || broken.attemptCount() != 1 || healthy.attemptCount() != 1 {
		t.Fatalf("got %+v, broken %d, healthy %d", response, broken.attemptCount(), healthy.attemptCount())
	}

	broken.lock.Lock()
	defer broken.lock.Unlock()

	if broken.unreachable != 1 {
		t.Fatalf("broken exit must be marked unreachable once, got %d", broken.unreachable)
	}

	if healthy.attempts[0].Retries != 0 {
		t.Fatal("requeue must not count as a retry")
	}
}

func TestSchedulerTooManyRequestsBacksOff(t *testing.T) {
	scheduler := newTestScheduler(1)
	exit := newFakeExit("only", respondSequence(
		&Response{Status: ResponseStatusOk, Code: http.StatusTooManyRequests},
		&Response{Status: ResponseStatusOk, Code: http.StatusOK},
	))
	startScheduler(t, scheduler, exit)

	response := execute(t, scheduler, "https://a.test/", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("got %+v", response)
	}

	exit.lock.Lock()
	defer exit.lock.Unlock()

	if !slices.Equal(exit.backOffs, []string{"a.test"}) {
		t.Fatalf("429 must back the exit off the host, got %v", exit.backOffs)
	}
}

func TestSchedulerWaitsForProxies(t *testing.T) {
	scheduler := newTestScheduler(0)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := scheduler.start(ctx)
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	target := mustParseUrl(t, "https://a.test/")
	result := make(chan *Response, 1)
	go func() {
		response, err := scheduler.execute(context.Background(), target, 0, nil, nil)
		if err != nil {
			t.Error(err)
		}

		result <- response
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-result:
		t.Fatal("no exit exists yet")
	default:
	}

	scheduler.setProxies([]proxyExit{newFakeExit("late", respondWith(201))})

	select {
	case response := <-result:
		if response.Code != 201 {
			t.Fatalf("got %+v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request must run once an exit appears")
	}
}

func TestSchedulerRateLimitedRequestRunsLater(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondWith(200))
	exit.limited = true
	exit.retryAfter = 5 * time.Millisecond
	startScheduler(t, scheduler, exit)

	time.AfterFunc(50*time.Millisecond, func() {
		exit.lock.Lock()
		exit.limited = false
		exit.lock.Unlock()
	})

	if response := execute(t, scheduler, "https://a.test/", nil, nil); response.Code != 200 {
		t.Fatalf("got %+v", response)
	}
}

func TestSchedulerCancellation(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		scheduler := newTestScheduler(0)
		finished := make(chan interface{}, 1)
		scheduler.finished.Register(finished)
		startScheduler(t, scheduler)

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)

		response, err := scheduler.execute(ctx, mustParseUrl(t, "https://a.test/"), 0, nil, nil)
		if err != nil || response.Status != ResponseStatusRequestCancelled {
			t.Fatalf("got %+v %v", response, err)
		}

		select {
		case request := <-finished:
			if request.(*ActiveRequest).Status != RequestStatusFinished {
				t.Fatal("dropped request must be finished")
			}
		case <-time.After(time.Second):
			t.Fatal("scheduler must drop the cancelled request")
		}
	})

	t.Run("in flight", func(t *testing.T) {
		scheduler := newTestScheduler(3)
		finished := make(chan interface{}, 1)
		scheduler.finished.Register(finished)
		exit := newFakeExit("hung", waitForContext)
		startScheduler(t, scheduler, exit)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		response, err := scheduler.execute(ctx, mustParseUrl(t, "https://a.test/"), 0, nil, nil)
		if err != nil || response.Status != ResponseStatusRequestCancelled {
			t.Fatalf("got %+v %v", response, err)
		}

		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("aborted attempt must finish the request")
		}

		if exit.attemptCount() != 1 {
			t.Fatalf("cancelled requests are not retried, got %d attempts", exit.attemptCount())
		}
	})
}

func TestSchedulerPassesRequestFields(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondWith(200))
	startScheduler(t, scheduler, exit)

	warmup := mustParseUrl(t, "https://a.test/warm")
	if _, err := scheduler.execute(context.Background(), mustParseUrl(t, "https://a.test/x"), 42, []uint16{503}, warmup); err != nil {
		t.Fatal(err)
	}

	request := exit.attempts[0]
	if request.Priority != 42 || request.WarmupUrl != warmup || !slices.Equal(request.RetryOnCodes, []uint16{503}) {
		t.Fatalf("unexpected request %+v", request)
	}
}

func TestSchedulerManyRequests(t *testing.T) {
	scheduler := newRequestScheduler(1, time.Second)
	scheduler.idleInterval = 10 * time.Millisecond

	exits := []proxyExit{
		newFakeExit("a", respondWith(200)),
		newFakeExit("b", respondSequence(&Response{Status: ResponseStatusOk, Code: 502}, &Response{Status: ResponseStatusOk, Code: 200})),
		newFakeExit("c", respondWith(200)),
	}
	startScheduler(t, scheduler, exits...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var group sync.WaitGroup
	for index := range 100 {
		target := &url.URL{Scheme: "https", Host: fmt.Sprintf("h%d.test", index%7), Path: "/"}

		group.Go(func() {
			response, err := scheduler.execute(ctx, target, 0, nil, nil)
			if err != nil || response.Status != ResponseStatusOk {
				t.Errorf("request %d: %+v %v", index, response, err)
			}
		})
	}

	group.Wait()
}

func TestSchedulerStopsWhileBroadcasting(t *testing.T) {
	scheduler := newTestScheduler(0)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := scheduler.start(ctx)

	cancel()

	// Submissions racing the shutdown must not block it.
	for range 5 {
		go scheduler.requests.Submit(queuedRequest(0, 0, context.Background()))
		go scheduler.setProxies(nil)
	}

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}
