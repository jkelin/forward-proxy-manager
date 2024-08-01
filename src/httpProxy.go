package main

import (
	"bytes"
	"context"
	"github.com/go-httpproxy/httpproxy"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func OnError(ctx *httpproxy.Context, where string, err *httpproxy.Error, opErr error) {
	if strings.Contains(opErr.Error(), "request canceled") {
		return
	}

	if strings.Contains(opErr.Error(), "request canceled") {
		return
	}

	log.Printf("ERR: %s: %s [%s]", where, err, opErr)
}

//func OnAuth(ctx *httpproxy.Context, authType string, user string, pass string) bool {
//	// Auth test user.
//	if user == "test" && pass == "1234" {
//		return true
//	}
//	return false
//}

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

func OnRequest(ctx *httpproxy.Context, req *http.Request) (
	resp *http.Response) {
	// Log proxying requests.
	//log.Printf("INFO: Proxy: %s %s", req.Method, req.URL.String())

	priority := int64(0)
	prioHeader, err := strconv.ParseInt(req.Header.Get("x-priority"), 10, 32)
	if err == nil {
		priority = prioHeader
		req.Header.Del("x-priority")
	}

	retryOnCodes := make([]uint16, 0)

	_, respChan, err := initializeRequest(req.URL, priority, retryOnCodes, req.Context())
	if err != nil {
		log.Printf("ERROR %s: %v", req.URL.String(), err)
		return createStringResp("Proxy error", 500)
	}

	proxiedResp := <-respChan
	if proxiedResp.Status == ResponseStatusRequestCancelled {
		return nil
	}

	if proxiedResp.Status == ResponseStatusTimeout {
		return createStringResp("Remote host timed out", 502)
	}

	if proxiedResp.Status == ResponseStatusHostUnreachable {
		return createStringResp("Remote host unreachable", 502)
	}

	reader := bytes.NewReader(proxiedResp.Body)

	response := http.Response{
		Body:             io.NopCloser(reader),
		StatusCode:       proxiedResp.Code,
		TransferEncoding: nil,
		Uncompressed:     true,
		ContentLength:    int64(len(proxiedResp.Body)),
		Close:            false,
		Header:           http.Header{},
	}

	for key, values := range proxiedResp.Headers {
		if strings.ToLower(key) == "transfer-encoding" || strings.ToLower(key) == "content-length" {
			continue
		}

		for _, value := range values {
			response.Header.Set(key, value)
		}
	}

	response.ContentLength = -1
	response.TransferEncoding = nil

	return &response
}

func runHttpProxy(ctx context.Context) {
	prx, _ := httpproxy.NewProxy()

	prx.OnError = OnError
	//prx.OnAccept = OnAccept
	//prx.OnAuth = OnAuth
	prx.OnConnect = OnConnect
	prx.OnRequest = OnRequest

	err := http.ListenAndServe(":8080", prx)
	if err != nil {
		log.Fatalf("Failed to start proxy server: %v", err)
	}
}
