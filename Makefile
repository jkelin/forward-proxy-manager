include .env
.PHONY: build publish codegen

build: codegen
	docker build -t ghcr.io/jkelin/forward-proxy-manager:latest ./src

publish: build
	docker push ghcr.io/jkelin/forward-proxy-manager:latest

codegen:
	protoc --go_out=src --go-grpc_out=src service.proto
