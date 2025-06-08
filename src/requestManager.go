package main

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/dustin/go-broadcast"
	"github.com/google/btree"
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
	RequestStatusPending ResponseStatus = iota
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
}

var requestCounter uint64 = 0
var newRequestsBroacast = broadcast.NewBroadcaster(1)
var requestFinishedBroacast = broadcast.NewBroadcaster(1)

func initializeRequest(
	uri *url.URL,
	priority int64,
	retryOnCodes []uint16,
	ctx context.Context,
) (*ActiveRequest, <-chan *Response, error) {
	hostInfo := getHostInfo(uri.Hostname())
	if !hostInfo.isOnline() {
		return nil, nil, errors.New("host is not reachable")
	}

	requestCounter = requestCounter + 1
	callback := make(chan *Response, 1)

	req := &ActiveRequest{
		Id:           requestCounter - 1,
		Url:          uri,
		Method:       "GET",
		Priority:     priority,
		Host:         *hostInfo,
		Status:       RequestStatus(RequestStatusPending),
		Retries:      0,
		Callback:     callback,
		Context:      ctx,
		RetryOnCodes: retryOnCodes,
	}

	newRequestsBroacast.Submit(req)

	return req, callback, nil
}

func (request *ActiveRequest) executeAt(proxy *ProxyClient) {
	request.Lock.Lock()
	request.Status = RequestStatus(RequestStatusActive)
	request.Lock.Unlock()

	resp, err := proxy.makeRequestWithClient(request, globalConfiguration.RequestTimeout)

	request.Lock.Lock()
	defer request.Lock.Unlock()
	defer func() {
		if request.Status == RequestStatus(RequestStatusPending) {
			newRequestsBroacast.Submit(request)
		} else {
			requestFinishedBroacast.Submit(request)
		}
	}()

	retry := false

	if err != nil {
		if urlErr, ok := err.(*url.Error); ok && urlErr.Err.Error() == "EOF" {
			retry = true
		} else if err.Error() == "context canceled" {
			request.Callback <- &Response{
				Status: ResponseStatusRequestCancelled,
			}

			return
		} else {
			log.Printf("UNKNOWN ERROR %s: %v", request.Url, err)
			request.Callback <- &Response{
				Status: ResponseStatusUnknownError,
			}

			return
		}
	} else {
		if resp.Status == ResponseStatusTimeout || resp.Status == ResponseStatusHostUnreachable || (resp.Status == ResponseStatusOk && resp.Code == 502) || (resp.Status == ResponseStatusOk && resp.Code == 0) {
			retry = true
		} else if resp.Status == ResponseStatusProxyUnreachable {
			proxy.markUnreachable()

			request.Status = RequestStatus(RequestStatusPending)

			return
		} else if resp.Code == 429 {
			_, _, err = proxy.limiter.RateLimit(request.Host.host, 100)
			if err != nil {
				log.Printf("UNKNOWN ERROR in rateLimiter %s: %v", request.Url, err)

				request.Callback <- &Response{
					Status: ResponseStatusUnknownError,
				}

				return
			}

			retry = true
		}

		for _, code := range request.RetryOnCodes {
			if resp.Code == int(code) {
				retry = true
				break
			}
		}
	}

	if retry && int(request.Retries) < globalConfiguration.Retries {
		request.Retries = request.Retries + 1
		request.Status = RequestStatus(RequestStatusPending)

		return
	}

	request.Callback <- resp
}

// shuffleIndices returns a slice of indices in random order
func shuffleIndices(length int) []int {
	indices := make([]int, length)
	for i := range indices {
		indices[i] = i
	}
	rand.Shuffle(length, func(i, j int) {
		indices[i], indices[j] = indices[j], indices[i]
	})
	return indices
}

func runRequestScheduler(ctx context.Context) {
	pq := btree.NewG[*ActiveRequest](32, func(a, b *ActiveRequest) bool {
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}

		return a.Id < b.Id
	})

	proxyListChanged := make(chan interface{})
	proxyListChangedBroadcaster.Register(proxyListChanged)
	defer proxyListChangedBroadcaster.Unregister(proxyListChanged)

	newRequests := make(chan interface{})
	newRequestsBroacast.Register(newRequests)
	defer newRequestsBroacast.Unregister(newRequests)

	proxies := make([]*ProxyClient, 0)
	retryRequestAfter := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			return
		case req := <-newRequests:
			pq.ReplaceOrInsert(req.(*ActiveRequest))
			retryRequestAfter = 0
		case newProxies := <-proxyListChanged:
			proxies = newProxies.([]*ProxyClient)
			retryRequestAfter = 0
		case <-time.After(retryRequestAfter):
		}

		retryRequestAfter = 1000 * time.Millisecond

		if pq.Len() == 0 {
			continue
		}

		pq.Ascend(func(item *ActiveRequest) bool {
			if item.Context.Err() != nil {
				pq.Delete(item)
				return true
			}

			for _, idx := range shuffleIndices(len(proxies)) {
				proxy := proxies[idx]
				limited, result, err := proxy.RateLimit(item.Host)
				if err != nil {
					log.Fatal(err)
				}

				if limited {
					if result.RetryAfter < retryRequestAfter {
						retryRequestAfter = result.RetryAfter
					}

					continue
				}

				pq.Delete(item)
				go item.executeAt(proxy)
				retryRequestAfter = 0
				return false
			}

			return true
		})
	}
}
