package main

import (
	"context"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type GlobalConfiguration struct {
	ProxyListUrl           string        `split_words:"true" required:"true"`
	RequestTimeout         time.Duration `split_words:"true" default:"20s"`
	RetryTimeout           time.Duration `split_words:"true" default:"5s"`
	InitialIpInfoTimeout   time.Duration `split_words:"true" default:"10s"`
	Retries                int           `split_words:"true" default:"1"`
	HostInfoRequestTimeout time.Duration `split_words:"true" default:"5s"`
	ThrottleRequestsPerMin int           `split_words:"true" default:"30"`
	ThrottleRequestsBurst  int           `split_words:"true" default:"5"`
	UnreachableClientRetry time.Duration `split_words:"true" default:"60s"`
	EnableWeb              bool          `split_words:"true" default:"false"`
}

var globalConfiguration GlobalConfiguration

func main() {
	err := godotenv.Load()
	// if err != nil {
	// 	log.Fatalf("Error loading .env file")
	// }

	err = envconfig.Process("", &globalConfiguration)
	if err != nil {
		log.Fatal(err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())

	go runProxyManager(ctx)
	go runHttpProxy(ctx)
	go runGrpcProxy(ctx)
	go runRequestScheduler(ctx)
	if globalConfiguration.EnableWeb {
		go runWeb(ctx)
	}

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Printf("Closing")

	cancel()
}
