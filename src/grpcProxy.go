package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "scrape-proxy/com.scrape-proxy"
)

type server struct {
	pb.UnimplementedProxyServer
	scheduler *RequestScheduler
}

func createProxyErrorResp(errorType pb.ProxyResponseError_ErrorType) *pb.ProxyResponse {
	return &pb.ProxyResponse{
		Response: &pb.ProxyResponse_Error{
			Error: &pb.ProxyResponseError{
				ErrorType: errorType,
			},
		},
	}
}

// parseRetryOnCodes validates retry_on_codes.
// Edge cases: codes outside 100-599 cannot be HTTP status codes and are rejected; an empty list is valid.
func parseRetryOnCodes(codes []uint32) ([]uint16, error) {
	retryOnCodes := make([]uint16, 0, len(codes))

	for _, code := range codes {
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("retry_on_codes contains %d, not an HTTP status code", code)
		}

		retryOnCodes = append(retryOnCodes, uint16(code))
	}

	return retryOnCodes, nil
}

// successHeaders flattens response headers into the proto map with lowercase (HTTP/2 style) names.
// Edge cases: hop-by-hop framing headers are dropped; for repeated headers the last value wins.
func successHeaders(headers map[string][]string) map[string]string {
	flattened := make(map[string]string, len(headers))

	for key, values := range headers {
		name := strings.ToLower(key)
		if name == "transfer-encoding" || name == "content-length" || len(values) == 0 {
			continue
		}

		flattened[name] = values[len(values)-1]
	}

	return flattened
}

// SendRequest proxies one GET through the pool.
// Edge cases: an invalid url or warmup_url returns INVALID_URL; invalid retry_on_codes return InvalidArgument;
// a target failing the reachability check returns REMOTE_HOST_UNREACHABLE; client cancellation or deadline returns
// the matching gRPC status instead of a response.
func (s *server) SendRequest(ctx context.Context, in *pb.ProxyRequest) (*pb.ProxyResponse, error) {
	target, err := parseAbsoluteHttpUrl(in.GetUrl())
	if err != nil {
		return createProxyErrorResp(pb.ProxyResponseError_INVALID_URL), nil
	}

	warmupUrl, err := parseWarmupUrl(in.GetWarmupUrl())
	if err != nil {
		return createProxyErrorResp(pb.ProxyResponseError_INVALID_URL), nil
	}

	retryOnCodes, err := parseRetryOnCodes(in.GetRetryOnCodes())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	proxiedResp, err := s.scheduler.execute(ctx, target, in.GetPriority(), retryOnCodes, warmupUrl)
	if errors.Is(err, errHostOffline) {
		return createProxyErrorResp(pb.ProxyResponseError_REMOTE_HOST_UNREACHABLE), nil
	}

	if err != nil {
		log.Printf("ERROR %s: %v", target.String(), err)
		return createProxyErrorResp(pb.ProxyResponseError_PROXY_ERROR), nil
	}

	switch proxiedResp.Status {
	case ResponseStatusOk:
	case ResponseStatusRequestCancelled:
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}

		return nil, status.Error(codes.Canceled, "request cancelled")
	case ResponseStatusTimeout:
		return createProxyErrorResp(pb.ProxyResponseError_REMOTE_HOST_TIMED_OUT), nil
	case ResponseStatusHostUnreachable:
		return createProxyErrorResp(pb.ProxyResponseError_REMOTE_HOST_UNREACHABLE), nil
	default:
		return createProxyErrorResp(pb.ProxyResponseError_PROXY_ERROR), nil
	}

	return &pb.ProxyResponse{
		Response: &pb.ProxyResponse_Success{
			Success: &pb.ProxyResponseSuccess{
				Body:    proxiedResp.Body,
				Status:  int32(proxiedResp.Code),
				Headers: successHeaders(proxiedResp.Headers),
			},
		},
	}, nil
}

func newGrpcServer(scheduler *RequestScheduler) *grpc.Server {
	grpcServer := grpc.NewServer(
		grpc.MaxConcurrentStreams(math.MaxInt32),
		grpc.MaxSendMsgSize(math.MaxInt32),
		grpc.MaxRecvMsgSize(math.MaxInt32),
	)
	pb.RegisterProxyServer(grpcServer, &server{scheduler: scheduler})

	return grpcServer
}

// runGrpcProxy serves gRPC on port until ctx is done, then stops gracefully.
func runGrpcProxy(ctx context.Context, scheduler *RequestScheduler, port uint16) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to start gprc server: %v", err)
	}

	grpcServer := newGrpcServer(scheduler)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	log.Printf("grpc server listening at %v", listener.Addr())

	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("failed to serve grpc: %v", err)
	}
}
