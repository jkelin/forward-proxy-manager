image := "ghcr.io/jkelin/forward-proxy-manager:latest"

# List recipes
default:
    @just --list

# Install JS dependencies and git hooks
install:
    bun install

# Generate Go and TypeScript gRPC code from service.proto
codegen: codegen-go codegen-ts

# Generate Go gRPC code into src/com.scrape-proxy
codegen-go:
    protoc --go_out=./src --go-grpc_out=./src service.proto

# Generate TypeScript gRPC code into packages/client/src/generated
[working-directory("packages/client")]
codegen-ts:
    bun run --bun codegen

# Build the Go proxy binary, the challenge solver and the client package
build: build-go build-solver build-client

# Build the Go proxy binary into dist/
[working-directory("src")]
build-go:
    go build -o ../dist/forward-proxy-manager .

# Build the Cloudflare challenge solver (fpm-cfsolve) into solver/target/release
[working-directory("solver")]
build-solver:
    cargo build --release --locked

# Build the client package into packages/client/dist
[working-directory("packages/client")]
build-client:
    bun run build

# Run the Go proxy with variables from .env
[script]
dev:
    set -a
    . ./.env
    set +a
    go -C src run .

# Run Go (race detector) and client tests
test:
    bun run test

# Lint TypeScript (oxlint) and Go (go vet)
lint:
    bun run lint

# Format TypeScript/JSON/Markdown (oxfmt) and Go (gofmt)
format:
    bun run format

# Build the Docker image (context: repository root, for the solver and vendor/cfsolve)
docker-build:
    docker build -f src/Dockerfile -t {{ image }} .

# Push the Docker image and publish the client to npm
publish: publish-docker publish-client

# Push the Docker image
publish-docker: docker-build
    docker push {{ image }}

# Publish the client package to npm
[working-directory("packages/client")]
publish-client: build-client
    bun publish
