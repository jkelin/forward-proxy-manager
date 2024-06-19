// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.2.0
// - protoc             v4.25.1
// source: service.proto

package com_scrape_proxy

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// ProxyClient is the client API for Proxy service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type ProxyClient interface {
	SendRequest(ctx context.Context, in *ProxyRequest, opts ...grpc.CallOption) (*ProxyResponse, error)
}

type proxyClient struct {
	cc grpc.ClientConnInterface
}

func NewProxyClient(cc grpc.ClientConnInterface) ProxyClient {
	return &proxyClient{cc}
}

func (c *proxyClient) SendRequest(ctx context.Context, in *ProxyRequest, opts ...grpc.CallOption) (*ProxyResponse, error) {
	out := new(ProxyResponse)
	err := c.cc.Invoke(ctx, "/proxy.Proxy/SendRequest", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ProxyServer is the server API for Proxy service.
// All implementations must embed UnimplementedProxyServer
// for forward compatibility
type ProxyServer interface {
	SendRequest(context.Context, *ProxyRequest) (*ProxyResponse, error)
	mustEmbedUnimplementedProxyServer()
}

// UnimplementedProxyServer must be embedded to have forward compatible implementations.
type UnimplementedProxyServer struct {
}

func (UnimplementedProxyServer) SendRequest(context.Context, *ProxyRequest) (*ProxyResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method SendRequest not implemented")
}
func (UnimplementedProxyServer) mustEmbedUnimplementedProxyServer() {}

// UnsafeProxyServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to ProxyServer will
// result in compilation errors.
type UnsafeProxyServer interface {
	mustEmbedUnimplementedProxyServer()
}

func RegisterProxyServer(s grpc.ServiceRegistrar, srv ProxyServer) {
	s.RegisterService(&Proxy_ServiceDesc, srv)
}

func _Proxy_SendRequest_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ProxyRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(ProxyServer).SendRequest(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/proxy.Proxy/SendRequest",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(ProxyServer).SendRequest(ctx, req.(*ProxyRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// Proxy_ServiceDesc is the grpc.ServiceDesc for Proxy service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var Proxy_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "proxy.Proxy",
	HandlerType: (*ProxyServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "SendRequest",
			Handler:    _Proxy_SendRequest_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "service.proto",
}
