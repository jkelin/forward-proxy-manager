FROM golang:1.16-alpine

WORKDIR /app

RUN touch .env

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY *.go ./

RUN go build -o /app-build

EXPOSE 8080

CMD [ "/app-build" ]