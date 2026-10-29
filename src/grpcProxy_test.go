package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	pb "scrape-proxy/com.scrape-proxy"
)

// startGrpc serves newGrpcServer(scheduler) over an in-memory listener and returns a connected client.
func startGrpc(t *testing.T, scheduler *RequestScheduler) pb.ProxyClient {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	grpcServer := newGrpcServer(scheduler)

	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = connection.Close() })

	return pb.NewProxyClient(connection)
}

func sendGrpc(t *testing.T, client pb.ProxyClient, request *pb.ProxyRequest) *pb.ProxyResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	response, err := client.SendRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	return response
}

func errorType(t *testing.T, response *pb.ProxyResponse) pb.ProxyResponseError_ErrorType {
	t.Helper()

	if response.GetError() == nil {
		t.Fatalf("expected an error response, got %v", response)
	}

	return response.GetError().GetErrorType()
}

func TestParseRetryOnCodes(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		codes    []uint32
		expected []uint16
		valid    bool
	}{
		{"nil", nil, []uint16{}, true},
		{"empty", []uint32{}, []uint16{}, true},
		{"bounds", []uint32{100, 599}, []uint16{100, 599}, true},
		{"several", []uint32{503, 429, 403}, []uint16{503, 429, 403}, true},
		{"below range", []uint32{99}, nil, false},
		{"above range", []uint32{600}, nil, false},
		{"zero", []uint32{0}, nil, false},
		{"one bad among good", []uint32{503, 70000}, nil, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			codes, err := parseRetryOnCodes(testCase.codes)
			if (err == nil) != testCase.valid {
				t.Fatalf("got error %v, want valid %v", err, testCase.valid)
			}

			if testCase.valid && !slices.Equal(codes, testCase.expected) {
				t.Fatalf("got %v, want %v", codes, testCase.expected)
			}
		})
	}
}

func TestSuccessHeaders(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		headers  http.Header
		expected map[string]string
	}{
		{"nil", nil, map[string]string{}},
		{"lowercases", http.Header{"Content-Type": {"text/html"}, "X-Fpm-Warmup": {"fresh"}}, map[string]string{"content-type": "text/html", "x-fpm-warmup": "fresh"}},
		{"drops framing", http.Header{"Transfer-Encoding": {"chunked"}, "Content-Length": {"12"}, "Content-Disposition": {"inline"}}, map[string]string{"content-disposition": "inline"}},
		{"last value wins", http.Header{"Content-Type": {"a", "b"}}, map[string]string{"content-type": "b"}},
		{"empty values skipped", http.Header{"Content-Type": {}}, map[string]string{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if actual := successHeaders(testCase.headers); !maps.Equal(actual, testCase.expected) {
				t.Fatalf("got %v, want %v", actual, testCase.expected)
			}
		})
	}
}

func TestGrpcRejectsInvalidInput(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		request *pb.ProxyRequest
		code    codes.Code
	}{
		{"empty url", &pb.ProxyRequest{}, codes.OK},
		{"relative url", &pb.ProxyRequest{Url: "/search"}, codes.OK},
		{"scheme-less url", &pb.ProxyRequest{Url: "a.test/search"}, codes.OK},
		{"ftp url", &pb.ProxyRequest{Url: "ftp://a.test/"}, codes.OK},
		{"host-less url", &pb.ProxyRequest{Url: "https://:443/"}, codes.OK},
		{"bad port", &pb.ProxyRequest{Url: "https://a.test:0/"}, codes.OK},
		{"unparsable url", &pb.ProxyRequest{Url: "https://a.test/%zz"}, codes.OK},
		{"relative warmup", &pb.ProxyRequest{Url: "https://a.test/", WarmupUrl: proto.String("/warm")}, codes.OK},
		{"ftp warmup", &pb.ProxyRequest{Url: "https://a.test/", WarmupUrl: proto.String("ftp://a.test/")}, codes.OK},
		{"host-less warmup", &pb.ProxyRequest{Url: "https://a.test/", WarmupUrl: proto.String("https:///warm")}, codes.OK},
		{"warmup port out of range", &pb.ProxyRequest{Url: "https://a.test/", WarmupUrl: proto.String("https://a.test:70000/")}, codes.OK},
		{"retry code too low", &pb.ProxyRequest{Url: "https://a.test/", RetryOnCodes: []uint32{42}}, codes.InvalidArgument},
		{"retry code too high", &pb.ProxyRequest{Url: "https://a.test/", RetryOnCodes: []uint32{600}}, codes.InvalidArgument},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(0)
			exit := newFakeExit("only", respondWith(200))
			startScheduler(t, scheduler, exit)
			client := startGrpc(t, scheduler)

			response, err := client.SendRequest(context.Background(), testCase.request)
			if testCase.code != codes.OK {
				if status.Code(err) != testCase.code {
					t.Fatalf("got %v, want %v", err, testCase.code)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}

				if errorType(t, response) != pb.ProxyResponseError_INVALID_URL {
					t.Fatalf("got %v, want INVALID_URL", response)
				}
			}

			if exit.attemptCount() != 0 {
				t.Fatal("invalid input must never reach an exit")
			}
		})
	}
}

func TestGrpcStatusMapping(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		lookup   func(host string) *HostInfo
		respond  func(*ActiveRequest, int) (*Response, error)
		expected pb.ProxyResponseError_ErrorType
	}{
		{"host offline", offlineHost, respondWith(200), pb.ProxyResponseError_REMOTE_HOST_UNREACHABLE},
		{"timeout", onlineHost(true), respondSequence(&Response{Status: ResponseStatusTimeout}), pb.ProxyResponseError_REMOTE_HOST_TIMED_OUT},
		{"host unreachable", onlineHost(true), respondSequence(&Response{Status: ResponseStatusHostUnreachable}), pb.ProxyResponseError_REMOTE_HOST_UNREACHABLE},
		{"unknown status", onlineHost(true), respondSequence(&Response{Status: ResponseStatusUnknownError}), pb.ProxyResponseError_PROXY_ERROR},
		{"unknown error", onlineHost(true), func(*ActiveRequest, int) (*Response, error) { return nil, errors.New("boom") }, pb.ProxyResponseError_PROXY_ERROR},
		{"dropped connection", onlineHost(true), func(*ActiveRequest, int) (*Response, error) { return nil, fmt.Errorf("read: %w", io.ErrUnexpectedEOF) }, pb.ProxyResponseError_PROXY_ERROR},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newRequestScheduler(0, time.Second, testCase.lookup)
			scheduler.idleInterval = 10 * time.Millisecond
			startScheduler(t, scheduler, newFakeExit("only", testCase.respond))

			response := sendGrpc(t, startGrpc(t, scheduler), &pb.ProxyRequest{Url: "https://a.test/"})
			if actual := errorType(t, response); actual != testCase.expected {
				t.Fatalf("got %v, want %v", actual, testCase.expected)
			}
		})
	}
}

func TestGrpcSuccess(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondSequence(&Response{
		Status:  ResponseStatusOk,
		Code:    http.StatusNotFound,
		Body:    []byte("missing"),
		Headers: http.Header{"Content-Type": {"text/plain"}, "Content-Length": {"7"}},
	}))
	startScheduler(t, scheduler, exit)

	response := sendGrpc(t, startGrpc(t, scheduler), &pb.ProxyRequest{Url: "https://a.test/x?q=1"})
	success := response.GetSuccess()
	if success == nil || success.GetStatus() != http.StatusNotFound || string(success.GetBody()) != "missing" {
		t.Fatalf("got %v", response)
	}

	if !maps.Equal(success.GetHeaders(), map[string]string{"content-type": "text/plain"}) {
		t.Fatalf("got headers %v", success.GetHeaders())
	}

	request := exit.attempt(0)
	if request.Url.String() != "https://a.test/x?q=1" || request.WarmupUrl != nil || request.Priority != 0 || len(request.RetryOnCodes) != 0 {
		t.Fatalf("unexpected request %+v", request)
	}
}

func TestGrpcPassesRequestFields(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		priority *int64
		warmup   *string
	}{
		{"defaults", nil, nil},
		{"priority", proto.Int64(9), nil},
		{"negative priority", proto.Int64(-3), nil},
		{"empty warmup means none", nil, proto.String("")},
		{"warmup", proto.Int64(1), proto.String("https://a.test/warm")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(0)
			exit := newFakeExit("only", respondWith(200))
			startScheduler(t, scheduler, exit)

			sendGrpc(t, startGrpc(t, scheduler), &pb.ProxyRequest{Url: "https://a.test/", Priority: testCase.priority, WarmupUrl: testCase.warmup})

			request := exit.attempt(0)
			if request.Priority != (&pb.ProxyRequest{Priority: testCase.priority}).GetPriority() {
				t.Fatalf("got priority %d", request.Priority)
			}

			expectedWarmup := (&pb.ProxyRequest{WarmupUrl: testCase.warmup}).GetWarmupUrl()
			if (request.WarmupUrl == nil) != (expectedWarmup == "") || (request.WarmupUrl != nil && request.WarmupUrl.String() != expectedWarmup) {
				t.Fatalf("got warmup %v, want %q", request.WarmupUrl, expectedWarmup)
			}
		})
	}
}

func TestGrpcPriorityOrder(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("only", respondWith(200))
	exit.limited = true
	startScheduler(t, scheduler, exit)
	client := startGrpc(t, scheduler)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Count queued requests so all three wait behind the limited exit before it is freed one request at a time.
	published := make(chan interface{}, 10)
	scheduler.requests.Register(published)
	t.Cleanup(func() { scheduler.requests.Unregister(published) })

	done := make(chan struct{}, 3)
	for _, priority := range []int64{1, 5, 3} {
		go func() {
			if _, err := client.SendRequest(ctx, &pb.ProxyRequest{Url: "https://a.test/", Priority: proto.Int64(priority)}); err != nil {
				t.Error(err)
			}

			done <- struct{}{}
		}()
	}

	for range 3 {
		select {
		case <-published:
		case <-ctx.Done():
			t.Fatal("requests were not queued")
		}
	}

	// The scheduler loop may still be inserting the last request; one idle pass later the queue is complete.
	time.Sleep(50 * time.Millisecond)

	exit.lock.Lock()
	exit.limited = false
	exit.tokens = 1
	exit.lock.Unlock()

	for range 3 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("requests did not finish")
		}

		exit.lock.Lock()
		exit.tokens = 1
		exit.lock.Unlock()
	}

	exit.lock.Lock()
	defer exit.lock.Unlock()

	if !slices.Equal(exit.priorities, []int64{5, 3, 1}) {
		t.Fatalf("got order %v, want [5 3 1]", exit.priorities)
	}
}

func TestGrpcRetryOnCodes(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		codes    []uint32
		attempts int
		expected int32
	}{
		{"listed code retries", []uint32{503}, 2, 200},
		{"unlisted code returns", []uint32{429}, 1, 503},
		{"no codes returns", nil, 1, 503},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := newTestScheduler(1)
			exit := newFakeExit("only", respondSequence(
				&Response{Status: ResponseStatusOk, Code: 503, Headers: http.Header{}},
				&Response{Status: ResponseStatusOk, Code: 200, Headers: http.Header{}},
			))
			startScheduler(t, scheduler, exit)

			response := sendGrpc(t, startGrpc(t, scheduler), &pb.ProxyRequest{Url: "https://a.test/", RetryOnCodes: testCase.codes})
			if response.GetSuccess().GetStatus() != testCase.expected || exit.attemptCount() != testCase.attempts {
				t.Fatalf("got %v after %d attempts", response, exit.attemptCount())
			}
		})
	}
}

func TestGrpcCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		code    codes.Code
	}{
		{"client cancel", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)

			return ctx, cancel
		}, codes.Canceled},
		{"client deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, codes.DeadlineExceeded},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, queued := range []bool{true, false} {
				scheduler := newTestScheduler(0)
				exit := newFakeExit("hung", waitForContext)
				exit.limited = queued
				startScheduler(t, scheduler, exit)

				ctx, cancel := testCase.context()

				// Calling the handler directly checks the status the server itself returns.
				response, err := (&server{scheduler: scheduler}).SendRequest(ctx, &pb.ProxyRequest{Url: "https://a.test/"})
				cancel()

				if response != nil || status.Code(err) != testCase.code {
					t.Fatalf("queued %v: got %v %v, want %v", queued, response, err, testCase.code)
				}

				if !queued && exit.attemptCount() != 1 {
					t.Fatal("in-flight attempt expected")
				}
			}
		})
	}
}

func TestGrpcCancelledAttemptWithLiveCaller(t *testing.T) {
	scheduler := newTestScheduler(0)
	startScheduler(t, scheduler, newFakeExit("only", func(*ActiveRequest, int) (*Response, error) { return nil, context.Canceled }))

	response, err := (&server{scheduler: scheduler}).SendRequest(context.Background(), &pb.ProxyRequest{Url: "https://a.test/"})
	if response != nil || status.Code(err) != codes.Canceled {
		t.Fatalf("got %v %v", response, err)
	}
}

func TestGrpcCancellationOverWire(t *testing.T) {
	scheduler := newTestScheduler(0)
	exit := newFakeExit("hung", waitForContext)
	startScheduler(t, scheduler, exit)
	client := startGrpc(t, scheduler)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := client.SendRequest(ctx, &pb.ProxyRequest{Url: "https://a.test/"}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("got %v", err)
	}

	// The server must abort the attempt, not leave it hanging: the exit's request ctx ends with the stream.
	deadline := time.Now().Add(2 * time.Second)
	for exit.attemptCount() == 0 || exit.attempt(0).Context.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("in-flight attempt was not cancelled")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// warmupSite is a TLS+h2 target whose /warm sets a session cookie that /search requires.
func warmupSite(t *testing.T, warmStatus int) (*socksServer, string) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/warm", func(writer http.ResponseWriter, _ *http.Request) {
		if warmStatus != http.StatusOK {
			writer.WriteHeader(warmStatus)
			_, _ = writer.Write([]byte("warm-up blocked"))

			return
		}

		http.SetCookie(writer, &http.Cookie{Name: "session", Value: "1", Path: "/"})
		_, _ = writer.Write([]byte("home"))
	})
	mux.HandleFunc("/search", func(writer http.ResponseWriter, request *http.Request) {
		if _, err := request.Cookie("session"); err != nil {
			writer.WriteHeader(http.StatusForbidden)
			return
		}

		_, _ = writer.Write([]byte("results"))
	})

	target := startTargetServer(t, mux)

	return startSocksServer(t, "user", "secret"), target.URL
}

func TestGrpcWarmupEndToEnd(t *testing.T) {
	socks, base := warmupSite(t, http.StatusOK)
	scheduler := newRequestScheduler(0, 5*time.Second, onlineHost(true))
	scheduler.idleInterval = 10 * time.Millisecond
	startScheduler(t, scheduler, newSocksProxyClient(t, socks, testLimits()))
	client := startGrpc(t, scheduler)

	cold := sendGrpc(t, client, &pb.ProxyRequest{Url: base + "/search"})
	if cold.GetSuccess().GetStatus() != http.StatusForbidden || cold.GetSuccess().GetHeaders()["x-fpm-warmup"] != "" {
		t.Fatalf("without warmup_url no session is made and no status reported: %v", cold)
	}

	for _, expected := range []string{"fresh", "reused"} {
		response := sendGrpc(t, client, &pb.ProxyRequest{Url: base + "/search", WarmupUrl: proto.String(base + "/warm")})
		success := response.GetSuccess()

		if success.GetStatus() != http.StatusOK || string(success.GetBody()) != "results" || success.GetHeaders()["x-fpm-warmup"] != expected {
			t.Fatalf("want %s: got %v", expected, response)
		}
	}
}

func TestGrpcWarmupFailedReturnsWarmupResponse(t *testing.T) {
	socks, base := warmupSite(t, http.StatusServiceUnavailable)
	scheduler := newRequestScheduler(0, 5*time.Second, onlineHost(true))
	scheduler.idleInterval = 10 * time.Millisecond
	startScheduler(t, scheduler, newSocksProxyClient(t, socks, testLimits()))

	response := sendGrpc(t, startGrpc(t, scheduler), &pb.ProxyRequest{Url: base + "/search", WarmupUrl: proto.String(base + "/warm")})
	success := response.GetSuccess()

	if success.GetStatus() != http.StatusServiceUnavailable || string(success.GetBody()) != "warm-up blocked" || success.GetHeaders()["x-fpm-warmup"] != "failed" {
		t.Fatalf("got %v", response)
	}
}

func TestGrpcTransportFailuresEndToEnd(t *testing.T) {
	hang := make(chan struct{})
	slow := startTargetServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-hang:
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hang) })

	for _, testCase := range []struct {
		name     string
		url      string
		expected pb.ProxyResponseError_ErrorType
	}{
		{"target timeout", slow.URL + "/", pb.ProxyResponseError_REMOTE_HOST_TIMED_OUT},
		{"target refused", "https://" + closedPort(t) + "/", pb.ProxyResponseError_REMOTE_HOST_UNREACHABLE},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			socks := startSocksServer(t, "user", "secret")
			scheduler := newRequestScheduler(0, 300*time.Millisecond, onlineHost(true))
			scheduler.idleInterval = 10 * time.Millisecond
			startScheduler(t, scheduler, newSocksProxyClient(t, socks, testLimits()))

			response := sendGrpc(t, startGrpc(t, scheduler), &pb.ProxyRequest{Url: testCase.url})
			if actual := errorType(t, response); actual != testCase.expected {
				t.Fatalf("got %v, want %v", actual, testCase.expected)
			}
		})
	}
}
