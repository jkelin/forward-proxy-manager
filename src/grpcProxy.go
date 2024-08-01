package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"math"
	"net"
	url2 "net/url"
	"strings"

	"google.golang.org/grpc"
	pb "scrape-proxy/com.scrape-proxy"
)

type server struct {
	pb.UnimplementedProxyServer
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

func (s *server) SendRequest(ctx context.Context, in *pb.ProxyRequest) (*pb.ProxyResponse, error) {
	//return &pb.ProxyResponse{Message: "Hello " + in.GetName()}, nil

	priority := int64(0)
	if in.Priority != nil {
		priority = in.GetPriority()
	}

	url, err := url2.Parse(in.GetUrl())
	if err != nil || url.Scheme == "" || url.Host == "" {
		return createProxyErrorResp(pb.ProxyResponseError_INVALID_URL), nil
	}

	retryOnCodes := make([]uint16, 0)
	if in.RetryOnCodes != nil {
		for _, code := range in.GetRetryOnCodes() {
			retryOnCodes = append(retryOnCodes, uint16(code))
		}
	}

	_, respChan, err := initializeRequest(url, priority, retryOnCodes, ctx)
	if err != nil {
		log.Printf("ERROR %s: %v", url.String(), err)
		return createProxyErrorResp(pb.ProxyResponseError_PROXY_ERROR), nil
	}

	proxiedResp := <-respChan
	if proxiedResp.Status == ResponseStatusRequestCancelled {
		return nil, nil
	}

	if proxiedResp.Status == ResponseStatusTimeout {
		return createProxyErrorResp(pb.ProxyResponseError_REMOTE_HOST_TIMED_OUT), nil
	}

	if proxiedResp.Status == ResponseStatusHostUnreachable {
		return createProxyErrorResp(pb.ProxyResponseError_REMOTE_HOST_UNREACHABLE), nil
	}

	reader := bytes.NewReader(proxiedResp.Body)

	body, err := io.ReadAll(reader)
	if err != nil {
		log.Printf("ERROR %s: %v", url.String(), err)
		return createProxyErrorResp(pb.ProxyResponseError_PROXY_ERROR), nil
	}

	headers := make(map[string]string)

	for key, values := range proxiedResp.Headers {
		if strings.ToLower(key) == "transfer-encoding" || strings.ToLower(key) == "content-length" {
			continue
		}

		for _, value := range values {
			headers[key] = value
		}
	}

	response := &pb.ProxyResponse{
		Response: &pb.ProxyResponse_Success{
			Success: &pb.ProxyResponseSuccess{
				Body:    body,
				Status:  int32(proxiedResp.Code),
				Headers: headers,
			},
		},
	}

	return response, nil
}

func runGrpcProxy(ctx context.Context) {
	flag.Parse()
	lis, err := net.Listen("tcp", ":8082")
	if err != nil {
		log.Fatalf("failed to start gprc server: %v", err)
	}

	grpc.MaxCallSendMsgSize(math.MaxInt32)
	grpc.MaxCallRecvMsgSize(math.MaxInt32)
	grpc.MaxConcurrentStreams(math.MaxInt32)
	grpc.MaxSendMsgSize(math.MaxInt32)
	grpc.MaxRecvMsgSize(math.MaxInt32)

	s := grpc.NewServer()
	pb.RegisterProxyServer(s, &server{})

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve grpc: %v", err)
	} else {
		log.Printf("grpc server listening at %v", lis.Addr())
	}
}
