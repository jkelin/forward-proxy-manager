package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/dustin/go-broadcast"
	"github.com/inhies/go-bytesize"
	"github.com/throttled/throttled"
	"github.com/throttled/throttled/store/memstore"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/semaphore"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
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

	request, err := http.NewRequest(req.Method, req.Url.String(), nil)
	if err != nil {
		return nil, err
	}

	for key, values := range client.headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	request = request.WithContext(requestCtx)

	httpClient := client.httpClient
	if req.Host.supportsH2 {
		httpClient = client.http2Client
	}

	resp, err := httpClient.Do(request)
	if err != nil {
		return client.handleError(req, req.Url, req.Context, err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("Error closing body in makeRequestWithClient: %v", err)
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
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

func (client *ProxyClient) markUnreachable() {
	log.Printf("Marking client %s unrachable", client.id)
	now := time.Now()
	client.lastUnreachableAt = &now
}

func getFakeHeaders() http.Header {
	headers := http.Header{}

	acceptLanguage := "en-US,en;q=0.5"

	if rand.Float32() > 0.3 {
		languages := []string{"cs", "de", "es", "fr", "it", "ja", "ko", "nl", "pl", "pt", "ru", "tr", "zh"}
		acceptLanguage = fmt.Sprintf("en-US,en;q=0.9,%s;q=0.8", languages[rand.Intn(len(languages))])
	}

	if rand.Float32() > 0.8 {
		// firefox headers
		headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		headers.Set("Accept-Language", acceptLanguage)

		if rand.Float32() > 0.8 {
			headers.Set("Dnt", "1")
		}

		headers.Set("Sec-Fetch-Dest", `document`)
		headers.Set("Sec-Fetch-Mode", `navigate`)
		headers.Set("Sec-Fetch-Site", `none`)
		headers.Set("Sec-Fetch-User", `?1`)
		headers.Set("Upgrade-Insecure-Requests", `1`)

		version := 120 + rand.Intn(5)

		headers.Set("User-Agent", fmt.Sprintf(`Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%d.0) Gecko/20100101 Firefox/%d.0`, version, version))
	} else {
		// chrome
		headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		headers.Set("Accept-Language", acceptLanguage)

		if rand.Float32() > 0.2 {
			headers.Set("Dnt", "1")
		}

		version := 120 + rand.Intn(5)

		headers.Set("Sec-Ch-Ua", fmt.Sprintf(`"Google Chrome";v="%d", "Not:A-Brand";v="8", "Chromium";v="%d"`, version, version))
		headers.Set("Sec-Ch-Ua-Mobile", `?0`)
		headers.Set("Sec-Ch-Ua-Platform", `"Windows"`)
		headers.Set("Sec-Fetch-Dest", `document`)
		headers.Set("Sec-Fetch-Mode", `navigate`)
		headers.Set("Sec-Fetch-Site", `none`)
		headers.Set("Sec-Fetch-User", `?1`)
		headers.Set("Upgrade-Insecure-Requests", `1`)
		headers.Set("User-Agent", fmt.Sprintf(`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36`, version))
	}

	return headers
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

type ProxyClient struct {
	id                string
	httpClient        http.Client
	http2Client       http.Client
	headers           http.Header
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

			log.Printf("Connecting to proxy %s:%d", config.host, config.port)
			dialSocksProxy, err := proxy.SOCKS5(
				"tcp",
				fmt.Sprintf("%s:%d", config.host, config.port),
				&proxy.Auth{User: config.username, Password: config.password},
				proxy.Direct,
			)

			if err != nil {
				log.Printf("Error connecting to proxy: %v", err)
				return
			}
			http2Transport := &http.Transport{
				Dial:            dialSocksProxy.Dial,
				MaxIdleConns:    1024,
				MaxConnsPerHost: 12,
				IdleConnTimeout: 60 * time.Second,
			}

			err = http2.ConfigureTransport(http2Transport)
			if err != nil {
				log.Printf("error upgrading proxy to http2: %v", err)
				return
			}

			http2Client := http.Client{
				Transport: http2Transport,
				Timeout:   time.Second * 10,
			}

			httpClient := http.Client{
				Transport: &http.Transport{
					Dial:            dialSocksProxy.Dial,
					MaxIdleConns:    1024,
					MaxConnsPerHost: 6,
					IdleConnTimeout: 60 * time.Second,
				},
				Timeout: time.Second * 10,
			}

			myClient := ProxyClient{
				id:          config.host,
				httpClient:  httpClient,
				http2Client: http2Client,
				headers:     getFakeHeaders(),
				limiter:     createLimiter(),
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
