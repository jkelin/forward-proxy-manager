FROM golang:1.20-alpine  AS builder

WORKDIR /app

RUN touch .env

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY *.go ./
COPY templates /app/templates

RUN go build -o /build

FROM alpine:latest AS production
LABEL org.opencontainers.image.source="https://github.com/jkelin/forward-proxy-manager"

EXPOSE 8080
EXPOSE 8081
CMD ["./app"]

COPY --from=builder /build /app
