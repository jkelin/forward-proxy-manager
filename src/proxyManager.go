package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/dustin/go-broadcast"
	"github.com/inhies/go-bytesize"
	"github.com/throttled/throttled"
	"github.com/throttled/throttled/store/memstore"
	"golang.org/x/sync/semaphore"
)

type ProxyConfig struct {
	host     string
	port     int64
	username string
	password string
}

func (client *ProxyClient) handleError(req *ActiveRequest, uri *url.URL, mainCtx context.Context, err error) (*Response, error) {
	if mainCtx.Err() != nil {
		return nil, mainCtx.Err()
	}

	if strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout") {
		return &Response{
			Status: ResponseStatusTimeout,
		}, nil
	}

	var e *net.OpError
	if errors.As(err, &e) && e.Op == "socks connect" {
		log.Printf("%s %s %s %s", client.id, req.Method, uri.String(), "Proxy unreachable")
		return &Response{Status: ResponseStatusProxyUnreachable}, nil
	} else {
		log.Printf("%s %s %s %v", client.id, req.Method, uri.String(), err.Error())
		return nil, err
	}
}

func (client *ProxyClient) makeRequestWithClient(req *ActiveRequest, timeout time.Duration) (*Response, error) {
	start := time.Now()

	requestCtx, cancelFn := context.WithTimeout(req.Context, timeout)
	defer cancelFn()

	if req.Host.supportsHttps {
		req.Url.Scheme = "https"
	} else {
		req.Url.Scheme = "http"
	}

	req.Url.Host = withoutDefaultPort(req.Url)

	request, err := fhttp.NewRequestWithContext(requestCtx, req.Method, req.Url.String(), nil)
	if err != nil {
		return nil, err
	}

	request.Header = client.headers.Clone()

	// tls-client negotiates h2/http1.1 via the profile's ALPN, like the impersonated browser.
	resp, err := client.httpClient.Do(request)
	if err != nil {
		return client.handleError(req, req.Url, req.Context, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("Error closing body in makeRequestWithClient: %v", err)
		}
	}()

	// Fake headers set Accept-Encoding explicitly, so the transport leaves the body encoded unless it reports otherwise.
	contentEncoding := resp.Header.Get("Content-Encoding")
	if resp.Uncompressed {
		contentEncoding = ""
	}

	body, err := decodeResponseBody(resp.Body, contentEncoding)
	if err != nil {
		return client.handleError(req, req.Url, req.Context, err)
	}

	duration := time.Since(start)
	log.Printf("%dp %s %s %s %d %s, %dms", req.Priority, client.id, req.Method, req.Url.String(), resp.StatusCode, bytesize.New(float64(len(body))), duration.Milliseconds())

	mainResponse := Response{
		Status:  ResponseStatusOk,
		Code:    resp.StatusCode,
		Body:    body,
		Headers: http.Header{},
	}

	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		mainResponse.Headers.Set("Content-Type", contentType)
	}

	if contentType := resp.Header.Get("Content-Disposition"); contentType != "" {
		mainResponse.Headers.Set("Content-Disposition", contentType)
	}

	return &mainResponse, nil
}

// withoutDefaultPort drops the scheme's default port, as browsers omit it from Host/:authority.
// MITM CONNECT targets arrive as "host:443", which would otherwise leak as ":authority: host:443".
// Edge cases: non-default ports are kept; IPv6 literals keep their brackets.
func withoutDefaultPort(target *url.URL) string {
	port := target.Port()
	isDefaultPort := (target.Scheme == "https" && port == "443") || (target.Scheme == "http" && port == "80")
	if !isDefaultPort {
		return target.Host
	}

	hostname := target.Hostname()
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]"
	}

	return hostname
}

func (client *ProxyClient) markUnreachable() {
	log.Printf("Marking client %s unrachable", client.id)
	now := time.Now()
	client.lastUnreachableAt = &now
}

var proxyRegex = regexp.MustCompile(`(\d{1,3}\.\d{1,3}.\d{1,3}.\d{1,3}):(\d{1,5}):(.+?):(.+)(\r\n)`)

func getProxiesFromUrl(url string) ([]ProxyConfig, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("Error closing body in getProxiesFromUrl: %v", err)
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	matches := proxyRegex.FindAllStringSubmatch(string(body), -1)

	res := make([]ProxyConfig, 0)
	for _, v := range matches {
		port, err := strconv.ParseInt(v[2], 10, 32)
		if err != nil {
			return nil, err
		}

		res = append(res, ProxyConfig{
			host:     v[1],
			port:     port,
			username: v[3],
			password: v[4],
		})
	}

	return res, nil
}

// ProxyClient is one SOCKS exit with one stable browser identity: TLS/HTTP2 profile and matching headers.
type ProxyClient struct {
	id                string
	httpClient        tls_client.HttpClient
	headers           fhttp.Header
	limiter           *throttled.GCRARateLimiter
	lastUnreachableAt *time.Time
}

func (client *ProxyClient) RateLimit(host HostInfo) (bool, throttled.RateLimitResult, error) {
	if client.lastUnreachableAt != nil && client.lastUnreachableAt.Add(globalConfiguration.UnreachableClientRetry).Before(time.Now()) {
		return false, throttled.RateLimitResult{
			RetryAfter: time.Now().Sub(client.lastUnreachableAt.Add(globalConfiguration.UnreachableClientRetry)),
		}, nil
	}

	limited, result, err := client.limiter.RateLimit(host.host, 0)
	if limited {
		return limited, result, err
	}

	limited, result, err = client.limiter.RateLimit(host.host, 1)

	return limited, result, err
}

func createLimiter() *throttled.GCRARateLimiter {
	store, err := memstore.New(65536)
	if err != nil {
		log.Fatal(err)
	}

	quota := throttled.RateQuota{
		MaxRate:  throttled.PerMin(globalConfiguration.ThrottleRequestsPerMin),
		MaxBurst: globalConfiguration.ThrottleRequestsBurst,
	}

	rateLimiter, err := throttled.NewGCRARateLimiter(store, quota)
	if err != nil {
		log.Fatal(err)
	}

	return rateLimiter
}

func getExternalProxyIp(client *ProxyClient, ctx context.Context) (*string, error) {
	uri, _ := url.Parse("https://ifconfig.io/ip")
	ipConfigHostInfo := getHostInfo("ifconfig.io")
	req := &ActiveRequest{
		Id:       0,
		Url:      uri,
		Method:   "GET",
		Priority: 10000,
		Host:     *ipConfigHostInfo,
		Context:  ctx,
		Callback: nil,
		Lock:     sync.Mutex{},
	}
	ipResp, err := client.makeRequestWithClient(req, globalConfiguration.InitialIpInfoTimeout)
	if err != nil {
		return nil, err
	}

	ip := strings.Trim(string(ipResp.Body), "\n")

	return &ip, nil
}

var proxyListChangedBroadcaster = broadcast.NewBroadcaster(1)

func runProxyManager(ctx context.Context) {
	proxies, err := getProxiesFromUrl(globalConfiguration.ProxyListUrl)
	if err != nil {
		log.Fatal(err)
	}

	proxyConnSemaphore := semaphore.NewWeighted(20)

	readyProxies := make([]*ProxyClient, 0)

	for _, config := range proxies {
		err := proxyConnSemaphore.Acquire(ctx, 1)
		if err != nil {
			log.Fatal(err)
		}

		go func(config ProxyConfig) {
			defer proxyConnSemaphore.Release(1)

			profile := randomBrowserProfile()
			log.Printf("Connecting to proxy %s:%d as %s", config.host, config.port, profile.GetClientHelloStr())

			// socks5h resolves target hostnames on the proxy, matching the previous x/net/proxy dialer.
			proxyUrl := url.URL{
				Scheme: "socks5h",
				User:   url.UserPassword(config.username, config.password),
				Host:   net.JoinHostPort(config.host, strconv.FormatInt(config.port, 10)),
			}
			idleConnTimeout := 60 * time.Second

			options := []tls_client.HttpClientOption{
				tls_client.WithClientProfile(profile),
				tls_client.WithProxyUrl(proxyUrl.String()),
				tls_client.WithTimeoutSeconds(10),
				tls_client.WithTransportOptions(&tls_client.TransportOptions{
					MaxIdleConns:    1024,
					MaxConnsPerHost: 12,
					IdleConnTimeout: &idleConnTimeout,
				}),
			}
			if shufflesTlsExtensions(profile) {
				options = append(options, tls_client.WithRandomTLSExtensionOrder())
			}

			httpClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
			if err != nil {
				log.Printf("Error connecting to proxy: %v", err)
				return
			}

			myClient := ProxyClient{
				id:         config.host,
				httpClient: httpClient,
				headers:    getFakeHeaders(profile),
				limiter:    createLimiter(),
			}

			ip, err := getExternalProxyIp(&myClient, ctx)
			if err != nil {
				log.Printf("Error connecting to proxy: %v", err)
				return
			}

			myClient.id = *ip

			log.Printf("Proxy %s ready", *ip)

			readyProxies = append(readyProxies, &myClient)
			proxiesToSend := make([]*ProxyClient, len(readyProxies))

			copy(proxiesToSend, readyProxies)

			proxyListChangedBroadcaster.Submit(proxiesToSend)
		}(config)
	}
}
