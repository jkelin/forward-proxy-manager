package main

import (
	"context"
	"errors"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-broadcast"
	"github.com/google/btree"
	"github.com/throttled/throttled/v2"
)

type ResponseStatus int64

const (
	ResponseStatusOk ResponseStatus = iota
	ResponseStatusTimeout
	ResponseStatusHostUnreachable
	ResponseStatusProxyUnreachable
	ResponseStatusRequestCancelled
	ResponseStatusUnknownError
)

type RequestStatus int64

const (
	RequestStatusPending RequestStatus = iota
	RequestStatusActive
	RequestStatusFinished
)

type Response struct {
	Status  ResponseStatus
	Code    int
	Body    []byte
	Headers http.Header
}

type ActiveRequest struct {
	Id           uint64
	Url          *url.URL
	Method       string
	Priority     int64
	Host         HostInfo
	Status       RequestStatus
	Retries      uint32
	Context      context.Context
	Callback     chan<- *Response
	Lock         sync.Mutex
	RetryOnCodes []uint16
	// WarmupUrl (x-warmup-url) is visited first by proxies without a session for Url's host; nil disables sessions.
	WarmupUrl *url.URL
}

// errHostOffline means the target host failed the direct reachability check, so no proxy is tried.
var errHostOffline = errors.New("host is not reachable")

// proxyExit is the scheduler's view of one proxy exit; *ProxyClient implements it.
type proxyExit interface {
	// RateLimit reports whether host is limited on this exit; when it is not, one request token is consumed.
	RateLimit(host HostInfo) (bool, throttled.RateLimitResult, error)
	// backOff keeps this exit away from host after the host answered 429.
	backOff(host HostInfo)
	// markUnreachable keeps this exit out of scheduling after the proxy itself failed.
	markUnreachable()
	makeRequestWithClient(req *ActiveRequest, timeout time.Duration) (*Response, error)
}

// RequestScheduler queues requests by priority and runs each on a free proxy exit, retrying failed attempts.
// Every new or re-queued request is published on requests; every final one on finished.
type RequestScheduler struct {
	retries        int
	requestTimeout time.Duration
	lookupHost     func(host string) *HostInfo
	shuffle        func(length int) []int
	// idleInterval is the longest pause between dispatch passes when no event arrives.
	idleInterval time.Duration
	nextId       atomic.Uint64
	requests     broadcast.Broadcaster
	finished     broadcast.Broadcaster
	proxyLists   broadcast.Broadcaster
}

func newRequestScheduler(retries int, requestTimeout time.Duration, lookupHost func(host string) *HostInfo) *RequestScheduler {
	return &RequestScheduler{
		retries:        retries,
		requestTimeout: requestTimeout,
		lookupHost:     lookupHost,
		shuffle:        rand.Perm,
		idleInterval:   time.Second,
		requests:       broadcast.NewBroadcaster(1),
		finished:       broadcast.NewBroadcaster(1),
		proxyLists:     broadcast.NewBroadcaster(1),
	}
}

// setProxies replaces the exits the scheduler dispatches to.
func (scheduler *RequestScheduler) setProxies(proxies []proxyExit) {
	scheduler.proxyLists.Submit(proxies)
}

// submit queues a GET for uri; its final response arrives once on the returned channel.
// Edge cases: a host failing the reachability check returns errHostOffline without queueing.
func (scheduler *RequestScheduler) submit(
	ctx context.Context,
	uri *url.URL,
	priority int64,
	retryOnCodes []uint16,
	warmupUrl *url.URL,
) (*ActiveRequest, <-chan *Response, error) {
	hostInfo := scheduler.lookupHost(uri.Hostname())
	if !hostInfo.isOnline() {
		return nil, nil, errHostOffline
	}

	callback := make(chan *Response, 1)

	request := &ActiveRequest{
		Id:           scheduler.nextId.Add(1) - 1,
		Url:          uri,
		Method:       "GET",
		Priority:     priority,
		Host:         *hostInfo,
		Status:       RequestStatusPending,
		Callback:     callback,
		Context:      ctx,
		RetryOnCodes: retryOnCodes,
		WarmupUrl:    warmupUrl,
	}

	scheduler.requests.Submit(request)

	return request, callback, nil
}

// execute submits a request and waits for its final response.
// Edge cases: ctx cancellation returns a RequestCancelled response at once; the queued request is dropped on the
// next dispatch pass and an attempt in flight is aborted through the same ctx.
func (scheduler *RequestScheduler) execute(
	ctx context.Context,
	uri *url.URL,
	priority int64,
	retryOnCodes []uint16,
	warmupUrl *url.URL,
) (*Response, error) {
	_, callback, err := scheduler.submit(ctx, uri, priority, retryOnCodes, warmupUrl)
	if err != nil {
		return nil, err
	}

	select {
	case response := <-callback:
		return response, nil
	case <-ctx.Done():
		return &Response{Status: ResponseStatusRequestCancelled}, nil
	}
}

type attemptDecision int

const (
	// decisionFinish hands the response to the caller.
	decisionFinish attemptDecision = iota
	// decisionRetry re-queues the request while retries remain, else finishes with the response.
	decisionRetry
	// decisionRequeue re-queues without using a retry: the exit failed, not the request.
	decisionRequeue
)

// classifyAttempt decides what follows one attempt and which response the caller gets if it ends here.
// Edge cases: a dropped connection (EOF) retries; a cancelled caller finishes as RequestCancelled; other errors
// finish as UnknownError; a missing status code is treated like 502.
func classifyAttempt(request *ActiveRequest, response *Response, err error) (attemptDecision, *Response) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return decisionFinish, &Response{Status: ResponseStatusRequestCancelled}
		}

		failure := &Response{Status: ResponseStatusUnknownError}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return decisionRetry, failure
		}

		log.Printf("UNKNOWN ERROR %s: %v", request.Url, err)

		return decisionFinish, failure
	}

	switch response.Status {
	case ResponseStatusProxyUnreachable:
		return decisionRequeue, response
	case ResponseStatusTimeout, ResponseStatusHostUnreachable:
		return decisionRetry, response
	case ResponseStatusOk:
	default:
		return decisionFinish, response
	}

	retryable := response.Code == 0 ||
		response.Code == http.StatusBadGateway ||
		response.Code == http.StatusTooManyRequests ||
		isSessionRejected(response.Code, request.WarmupUrl != nil) ||
		slices.Contains(request.RetryOnCodes, uint16(response.Code))
	if retryable {
		return decisionRetry, response
	}

	return decisionFinish, response
}

// attempt runs request once on proxy, then re-queues it or delivers the final response.
// makeRequestWithClient already reset the exit's session after a rejection, so the retry starts fresh.
func (scheduler *RequestScheduler) attempt(request *ActiveRequest, proxy proxyExit) {
	request.Lock.Lock()
	request.Status = RequestStatusActive
	request.Lock.Unlock()

	response, err := proxy.makeRequestWithClient(request, scheduler.requestTimeout)
	decision, final := classifyAttempt(request, response, err)

	if response != nil {
		if response.Status == ResponseStatusProxyUnreachable {
			proxy.markUnreachable()
		} else if response.Status == ResponseStatusOk && response.Code == http.StatusTooManyRequests {
			proxy.backOff(request.Host)
		}
	}

	request.Lock.Lock()
	requeue := decision == decisionRequeue || (decision == decisionRetry && int(request.Retries) < scheduler.retries)
	if requeue {
		if decision == decisionRetry {
			request.Retries++
		}

		request.Status = RequestStatusPending
		request.Lock.Unlock()
		scheduler.requests.Submit(request)

		return
	}

	request.Status = RequestStatusFinished
	request.Lock.Unlock()

	request.Callback <- final
	scheduler.finished.Submit(request)
}

// drop finishes a queued request whose caller is gone.
func (scheduler *RequestScheduler) drop(request *ActiveRequest) {
	request.Lock.Lock()
	request.Status = RequestStatusFinished
	request.Lock.Unlock()

	request.Callback <- &Response{Status: ResponseStatusRequestCancelled}
	scheduler.finished.Submit(request)
}

// requestOrder sorts higher priority first, then older (lower Id) first.
func requestOrder(first *ActiveRequest, second *ActiveRequest) bool {
	if first.Priority != second.Priority {
		return first.Priority > second.Priority
	}

	return first.Id < second.Id
}

// dispatch starts every queued request that has a free exit, highest priority first, and returns the pause
// before the next pass.
// Edge cases: requests with a done ctx are dropped; exits are tried in shuffled order; the pause is the shortest
// positive rate-limit hint, capped at idleInterval.
func (scheduler *RequestScheduler) dispatch(queue *btree.BTreeG[*ActiveRequest], proxies []proxyExit) time.Duration {
	type assignment struct {
		request *ActiveRequest
		proxy   proxyExit
	}

	wait := scheduler.idleInterval
	cancelled := make([]*ActiveRequest, 0)
	assignments := make([]assignment, 0)

	queue.Ascend(func(request *ActiveRequest) bool {
		if request.Context.Err() != nil {
			cancelled = append(cancelled, request)
			return true
		}

		for _, index := range scheduler.shuffle(len(proxies)) {
			proxy := proxies[index]

			limited, result, err := proxy.RateLimit(request.Host)
			if err != nil {
				log.Fatal(err)
			}

			if limited {
				if result.RetryAfter > 0 && result.RetryAfter < wait {
					wait = result.RetryAfter
				}

				continue
			}

			assignments = append(assignments, assignment{request: request, proxy: proxy})

			return true
		}

		return true
	})

	for _, request := range cancelled {
		queue.Delete(request)
		scheduler.drop(request)
	}

	for _, started := range assignments {
		queue.Delete(started.request)
		go scheduler.attempt(started.request, started.proxy)
	}

	return wait
}

// start registers the scheduler's listeners, then owns the queue in a goroutine until ctx is done, dispatching
// after every event and at least every idleInterval. The returned channel closes once the loop has stopped.
// Edge cases: listeners are registered before start returns, so no request or proxy list submitted afterwards is
// lost; on shutdown the listeners are drained while unregistering, so a broadcaster mid-delivery cannot block it.
func (scheduler *RequestScheduler) start(ctx context.Context) <-chan struct{} {
	proxyLists := make(chan interface{})
	scheduler.proxyLists.Register(proxyLists)

	requests := make(chan interface{})
	scheduler.requests.Register(requests)

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		scheduler.loop(ctx, requests, proxyLists)

		unregistered := make(chan struct{})
		go func() {
			scheduler.requests.Unregister(requests)
			scheduler.proxyLists.Unregister(proxyLists)
			close(unregistered)
		}()

		for {
			select {
			case <-requests:
			case <-proxyLists:
			case <-unregistered:
				return
			}
		}
	}()

	return stopped
}

func (scheduler *RequestScheduler) loop(ctx context.Context, requests <-chan interface{}, proxyLists <-chan interface{}) {
	queue := btree.NewG(32, requestOrder)
	proxies := make([]proxyExit, 0)
	timer := time.NewTimer(scheduler.idleInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case request := <-requests:
			queue.ReplaceOrInsert(request.(*ActiveRequest))
		case list := <-proxyLists:
			proxies = list.([]proxyExit)
		case <-timer.C:
		}

		timer.Reset(scheduler.dispatch(queue, proxies))
	}
}
