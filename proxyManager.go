package main

import (
	"context"
	"fmt"
	"github.com/corpix/uarand"
	"github.com/dustin/go-broadcast"
	"github.com/inhies/go-bytesize"
	"github.com/throttled/throttled"
	"github.com/throttled/throttled/store/memstore"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/semaphore"
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

	if e, ok := err.(*net.OpError); ok && e.Op == "socks connect" {
		log.Printf("%s %s %s %s", client.id, req.Method, uri.String(), "Proxy unreachable")
		return &Response{Status: ResponseStatusProxyUnreachable}, nil
	} else {
		log.Printf("%s %s %s %s", client.id, req.Method, uri.String(), err.Error())
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
	defer resp.Body.Close()

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

	headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	headers.Set("Accept-Language", "en-US,en;q=0.5")
	headers.Set("User-Agent", uarand.GetRandom())

	return headers
}

func getProxiesFromUrl(url string) ([]ProxyConfig, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	rxp := regexp.MustCompile(`(\d{1,3}\.\d{1,3}.\d{1,3}.\d{1,3}):(\d{1,5}):(.+?):(.+)(\r\n)`)
	matches := rxp.FindAllStringSubmatch(string(body), -1)

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

func (proxy *ProxyClient) RateLimit(host HostInfo) (bool, throttled.RateLimitResult, error) {
	if proxy.lastUnreachableAt != nil && proxy.lastUnreachableAt.Add(globalConfiguration.UnreachableClientRetry).Before(time.Now()) {
		return false, throttled.RateLimitResult{
			RetryAfter: time.Now().Sub(proxy.lastUnreachableAt.Add(globalConfiguration.UnreachableClientRetry)),
		}, nil
	}

	limited, result, err := proxy.limiter.RateLimit(host.host, 0)
	if limited {
		return limited, result, err
	}

	limited, result, err = proxy.limiter.RateLimit(host.host, 1)

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
				fmt.Println("error upgrading proxy to http2:", err)
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
