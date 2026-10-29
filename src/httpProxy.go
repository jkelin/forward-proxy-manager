package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-httpproxy/httpproxy"
)

// statusClientClosedRequest is nginx's 499; the client never sees it because it already went away.
const statusClientClosedRequest = 499

func OnError(ctx *httpproxy.Context, where string, err *httpproxy.Error, opErr error) {
	if strings.Contains(opErr.Error(), "request canceled") {
		return
	}

	log.Printf("ERR: %s: %s [%s]", where, err, opErr)
}

func OnConnect(ctx *httpproxy.Context, host string) (
	ConnectAction httpproxy.ConnectAction, newHost string) {
	// Apply "Man in the Middle" to all ssl connections. Never change host.
	return httpproxy.ConnectMitm, host
}

func createStringResp(body string, code int) *http.Response {
	response := http.Response{
		StatusCode:       code,
		TransferEncoding: nil,
		Uncompressed:     true,
		Close:            false,
		ContentLength:    int64(len(body)),
		Header:           http.Header{},
		Body:             io.NopCloser(strings.NewReader(body)),
	}

	response.Header.Set("Content-Type", "text/plain")

	return &response
}

type httpProxyHandler struct {
	scheduler *RequestScheduler
}

// parsePriority reads the x-priority header.
// Edge cases: a missing or non-numeric value, or one outside int32, means priority 0.
func parsePriority(raw string) int64 {
	priority, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0
	}

	return priority
}

// OnRequest answers every proxied request itself; returning nil would make httpproxy send it directly, bypassing
// the proxy pool. x-* headers are FPM control headers; outbound requests use the identity's headers only, so they
// never leak.
func (handler *httpProxyHandler) OnRequest(ctx *httpproxy.Context, req *http.Request) (resp *http.Response) {
	priority := parsePriority(req.Header.Get("x-priority"))

	warmupUrl, err := parseWarmupUrl(req.Header.Get(warmupUrlHeader))
	if err != nil {
		return createStringResp(fmt.Sprintf("Invalid %s: %v", warmupUrlHeader, err), http.StatusBadRequest)
	}

	proxiedResp, err := handler.scheduler.execute(req.Context(), req.URL, priority, make([]uint16, 0), warmupUrl)
	if errors.Is(err, errHostOffline) {
		return createStringResp("Remote host unreachable", http.StatusBadGateway)
	}

	if err != nil {
		log.Printf("ERROR %s: %v", req.URL.String(), err)
		return createStringResp("Proxy error", http.StatusInternalServerError)
	}

	switch proxiedResp.Status {
	case ResponseStatusOk:
	case ResponseStatusRequestCancelled:
		return createStringResp("Request cancelled", statusClientClosedRequest)
	case ResponseStatusTimeout:
		return createStringResp("Remote host timed out", http.StatusBadGateway)
	case ResponseStatusHostUnreachable:
		return createStringResp("Remote host unreachable", http.StatusBadGateway)
	default:
		return createStringResp("Proxy error", http.StatusInternalServerError)
	}

	response := http.Response{
		Body:             io.NopCloser(bytes.NewReader(proxiedResp.Body)),
		StatusCode:       proxiedResp.Code,
		TransferEncoding: nil,
		Uncompressed:     true,
		ContentLength:    -1,
		Close:            false,
		Header:           http.Header{},
	}

	for key, values := range proxiedResp.Headers {
		if strings.EqualFold(key, "transfer-encoding") || strings.EqualFold(key, "content-length") {
			continue
		}

		for _, value := range values {
			response.Header.Set(key, value)
		}
	}

	return &response
}

func newHttpProxy(scheduler *RequestScheduler) (*httpproxy.Proxy, error) {
	proxy, err := httpproxy.NewProxy()
	if err != nil {
		return nil, err
	}

	handler := &httpProxyHandler{scheduler: scheduler}
	proxy.OnError = OnError
	proxy.OnConnect = OnConnect
	proxy.OnRequest = handler.OnRequest

	return proxy, nil
}

// runHttpProxy serves the forward proxy on port until ctx is done.
func runHttpProxy(ctx context.Context, scheduler *RequestScheduler, port uint16) {
	proxy, err := newHttpProxy(scheduler)
	if err != nil {
		log.Fatalf("Failed to create proxy server: %v", err)
	}

	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: proxy}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Failed to stop proxy server: %v", err)
		}
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Failed to start proxy server: %v", err)
	}
}
