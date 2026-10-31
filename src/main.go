package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

type GlobalConfiguration struct {
	ProxyListUrl           string        `split_words:"true" required:"true"`
	IpCheckUrl             string        `split_words:"true" default:"https://ifconfig.io/ip"`
	RequestTimeout         time.Duration `split_words:"true" default:"20s"`
	RetryTimeout           time.Duration `split_words:"true" default:"5s"`
	InitialIpInfoTimeout   time.Duration `split_words:"true" default:"10s"`
	Retries                int           `split_words:"true" default:"1"`
	ThrottleRequestsPerMin int           `split_words:"true" default:"30"`
	ThrottleRequestsBurst  int           `split_words:"true" default:"5"`
	UnreachableClientRetry time.Duration `split_words:"true" default:"60s"`
	EnableWeb              bool          `split_words:"true" default:"false"`
	ProxyPort              uint16        `split_words:"true" default:"8080"`
	WebPort                uint16        `split_words:"true" default:"8081"`
	GrpcPort               uint16        `split_words:"true" default:"8082"`
	// ChallengeSolver is the fpm-cfsolve binary (name or path); empty disables Cloudflare challenge solving.
	ChallengeSolver      string        `split_words:"true"`
	ChallengeProfile     string        `split_words:"true" default:"/profiles/safari-26-ios.json"`
	ChallengeTimeout     time.Duration `split_words:"true" default:"120s"`
	ChallengeConcurrency int64         `split_words:"true" default:"2"`
}

var globalConfiguration GlobalConfiguration

// loadChallengeConfig builds challenge solving from CHALLENGE_*; nil when CHALLENGE_SOLVER is empty.
// Invalid settings stop the service instead of silently disabling solving.
func loadChallengeConfig() *challengeConfig {
	if globalConfiguration.ChallengeSolver == "" {
		return nil
	}

	identity, err := loadSolverIdentity(globalConfiguration.ChallengeProfile)
	if err != nil {
		log.Fatalf("CHALLENGE_PROFILE: %v", err)
	}

	solver, err := newProcessSolver(
		globalConfiguration.ChallengeSolver,
		identity.profilePath,
		globalConfiguration.ChallengeTimeout,
		globalConfiguration.ChallengeConcurrency,
	)
	if err != nil {
		log.Fatalf("CHALLENGE_SOLVER: %v", err)
	}

	log.Printf("Cloudflare challenges solved by %s as %s", solver.binary, identity.profilePath)

	return &challengeConfig{solver: solver, identity: identity}
}

func main() {
	// A missing .env is fine: the environment alone may configure the service.
	_ = godotenv.Load()

	err := envconfig.Process("", &globalConfiguration)
	if err != nil {
		log.Fatal(err.Error())
	}

	if _, err := parseAbsoluteHttpUrl(globalConfiguration.ProxyListUrl); err != nil {
		log.Fatalf("PROXY_LIST_URL: %v", err)
	}

	ipCheckUrl, err := parseAbsoluteHttpUrl(globalConfiguration.IpCheckUrl)
	if err != nil {
		log.Fatalf("IP_CHECK_URL: %v", err)
	}

	if globalConfiguration.Retries < 0 {
		log.Fatalf("RETRIES must not be negative, got %d", globalConfiguration.Retries)
	}

	limits := exitLimits{
		requestsPerMin:     globalConfiguration.ThrottleRequestsPerMin,
		burst:              globalConfiguration.ThrottleRequestsBurst,
		unreachableRetry:   globalConfiguration.UnreachableClientRetry,
		rateLimitedBackoff: globalConfiguration.RetryTimeout,
		clock:              time.Now,
	}
	if _, err := newLimiter(limits.requestsPerMin, limits.burst, limits.clock); err != nil {
		log.Fatalf("THROTTLE_REQUESTS_PER_MIN / THROTTLE_REQUESTS_BURST: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	scheduler := newRequestScheduler(globalConfiguration.Retries, globalConfiguration.RequestTimeout)
	scheduler.start(ctx)

	manager := &proxyManager{
		listUrl:        globalConfiguration.ProxyListUrl,
		ipCheckUrl:     ipCheckUrl,
		ipCheckTimeout: globalConfiguration.InitialIpInfoTimeout,
		limits:         limits,
		newProfile:     randomBrowserProfile,
		publish:        scheduler.setProxies,
		maxConcurrency: 20,
		challenges:     loadChallengeConfig(),
	}

	go func() {
		if err := manager.run(ctx); err != nil {
			log.Fatal(err)
		}
	}()

	go runHttpProxy(ctx, scheduler, globalConfiguration.ProxyPort)
	go runGrpcProxy(ctx, scheduler, globalConfiguration.GrpcPort)
	if globalConfiguration.EnableWeb {
		go runWeb(ctx, scheduler, globalConfiguration.WebPort)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	<-signals

	log.Printf("Closing")

	cancel()
}
