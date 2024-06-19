build:
	docker build -t ghcr.io/jkelin/forward-proxy-manager:latest ./
publish: build
	docker push ghcr.io/jkelin/forward-proxy-manager:latest
codegen:
	protoc --go_out=. --go-grpc_out=. service.proto
