package main

import (
	"context"
	"errors"
	"github.com/dustin/go-broadcast"
	"github.com/google/btree"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
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
	Id       uint64
	Url      *url.URL
	Method   string
	Priority int64
	Host     HostInfo
	Status   RequestStatus
	Retries  uint32
	Context  context.Context
	Callback chan<- *Response
	Lock     sync.Mutex
}

var requestCounter uint64 = 0
var newRequestsBroacast = broadcast.NewBroadcaster(1)
var requestFinishedBroacast = broadcast.NewBroadcaster(1)

func initializeRequest(uri *url.URL, priority int64, ctx context.Context) (*ActiveRequest, <-chan *Response, error) {
	hostInfo := getHostInfo(uri.Hostname())
	if !hostInfo.isOnline() {
		return nil, nil, errors.New("host is not reachable")
	}

	requestCounter = requestCounter + 1
	callback := make(chan *Response, 1)

	req := &ActiveRequest{
		Id:       requestCounter - 1,
		Url:      uri,
		Method:   "GET",
		Priority: priority,
		Host:     *hostInfo,
		Status:   RequestStatus(RequestStatusPending),
		Retries:  0,
		Callback: callback,
		Context:  ctx,
	}

	newRequestsBroacast.Submit(req)

	return req, callback, nil
}

func (request *ActiveRequest) executeAt(proxy *ProxyClient) {
	request.Lock.Lock()
	request.Status = RequestStatus(RequestStatusActive)
	request.Lock.Unlock()

	resp, err := proxy.makeRequestWithClient(request, globalConfiguration.RequestTimeout)
	if err != nil {
		if err.Error() == "context canceled" {
			request.Callback <- &Response{
				Status: ResponseStatusRequestCancelled,
			}
		} else {
			println(err.Error())
			request.Callback <- &Response{
				Status: ResponseStatusUnknownError,
			}
		}

		return
	}

	request.Lock.Lock()
	defer request.Lock.Unlock()

	retry := false

	if resp.Status == ResponseStatusTimeout || resp.Status == ResponseStatusHostUnreachable || (resp.Status == ResponseStatusOk && resp.Code == 502) {
		retry = true
	} else if resp.Status == ResponseStatusProxyUnreachable {
		proxy.markUnreachable()
		request.Status = RequestStatus(RequestStatusPending)

		newRequestsBroacast.Submit(request)
	} else if resp.Code == 429 {
		proxy.limiter.RateLimit(request.Host.host, 100)
		retry = true
	} else {
		request.Status = RequestStatus(RequestStatusFinished)

		request.Callback <- resp
		requestFinishedBroacast.Submit(request)
	}

	if retry {
		if request.Retries < 3 {
			request.Retries = request.Retries + 1
			request.Status = RequestStatus(RequestStatusPending)

			newRequestsBroacast.Submit(request)
		} else {
			request.Status = RequestStatus(RequestStatusFinished)

			request.Callback <- resp
			requestFinishedBroacast.Submit(request)
		}
	}
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

			for _, proxy := range proxies {
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
