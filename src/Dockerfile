# Build context: the repository root (`just docker-build`), so the solver and vendor/cfsolve are visible.

FROM golang:1.27-bookworm AS go-builder

WORKDIR /app

RUN touch .env

COPY src/go.mod src/go.sum ./
RUN go mod download

COPY src/*.go ./
COPY src/com.scrape-proxy /app/com.scrape-proxy
COPY src/templates /app/templates

RUN CGO_ENABLED=0 go build -o /build

# fpm-cfsolve: BoringSSL (btls-sys) needs cmake, clang and libclang for bindgen.
FROM rust:1-bookworm AS solver-builder

RUN apt-get update \
    && apt-get install -y --no-install-recommends cmake clang libclang-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY vendor/cfsolve vendor/cfsolve
COPY solver solver

WORKDIR /app/solver
RUN cargo build --release --locked

FROM debian:bookworm-slim AS production
LABEL org.opencontainers.image.source="https://github.com/jkelin/forward-proxy-manager"

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

EXPOSE 8080
EXPOSE 8081
EXPOSE 8082

# Cloudflare challenge solving is on by default; set CHALLENGE_SOLVER= to disable it.
ENV CHALLENGE_SOLVER=/usr/local/bin/fpm-cfsolve

# A profile file reads its answers from the directory of the same name next to it.
COPY vendor/cfsolve/profiles /profiles
COPY --from=solver-builder /app/solver/target/release/fpm-cfsolve /usr/local/bin/fpm-cfsolve
COPY --from=go-builder /build /app

CMD ["/app"]
