package main

import (
	"context"
	"fmt"
	"github.com/corpix/uarand"
	"github.com/dustin/go-broadcast"
	"github.com/inhies/go-bytesize"
	pq "github.com/jupp0r/go-priority-queue"
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

type RequestConfig struct {
	Url      string
	Method   string
	Priority float64
}

type ProxyConfig struct {
	host     string
	port     int64
	username string
	password string
}

type ResponseStatus int64

const (
	ResponseStatusOk ResponseStatus = iota
	ResponseStatusTimeout
	ResponseStatusHostUnreachable
	ResponseStatusProxyUnreachable
)

type Response struct {
	Status  ResponseStatus
	Code    int
	Body    []byte
	Headers http.Header
}

func handleError(client *ProxyClient, req *RequestConfig, uri *url.URL, mainCtx context.Context, err error) (*Response, error) {
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

func makeRequestWithClient(client *ProxyClient, host *HostInfo, req *RequestConfig, ctx context.Context, timeout time.Duration, priority float64) (*Response, error) {
	start := time.Now()

	requestCtx, cancelFn := context.WithTimeout(ctx, timeout)
	defer cancelFn()

	uri, err := url.Parse(req.Url)
	if err != nil {
		return nil, err
	}

	if host.supportsHttps {
		uri.Scheme = "https"
	} else {
		uri.Scheme = "http"
	}

	request, err := http.NewRequest(req.Method, uri.String(), nil)
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
	if host.supportsH2 {
		httpClient = client.http2Client
	}

	resp, err := httpClient.Do(request)
	if err != nil {
		return handleError(client, req, uri, ctx, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return handleError(client, req, uri, ctx, err)
	}

	duration := time.Since(start)
	log.Printf("%.0fp %s %s %s %d %s, %dms", priority, client.id, req.Method, uri.String(), resp.StatusCode, bytesize.New(float64(len(body))), duration.Milliseconds())

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

var clients = make([]*ProxyClient, 0)
var clientsMutex = sync.Mutex{}
var newClientBroadcast = broadcast.NewBroadcaster(1)

func addNewClient(client *ProxyClient) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	clients = append(clients, client)
	newClientBroadcast.Submit(nil)
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
	ipConfigHostInfo := getHostInfo("ifconfig.io")
	ipResp, err := makeRequestWithClient(client, ipConfigHostInfo, &RequestConfig{Method: "GET", Url: "https://ifconfig.io/ip"}, ctx, globalConfiguration.InitialIpInfoTimeout, 1337)
	if err != nil {
		return nil, err
	}

	ip := strings.Trim(string(ipResp.Body), "\n")

	return &ip, nil
}

func runProxyManager(ctx context.Context) {
	proxies, err := getProxiesFromUrl(globalConfiguration.ProxyListUrl)
	if err != nil {
		log.Fatal(err)
	}

	proxyConnSemaphore := semaphore.NewWeighted(20)

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

			addNewClient(&myClient)
		}(config)
	}
}

func tryGetClient(host *HostInfo) (client *ProxyClient, retryAfter *time.Duration) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	for {
		var bestResult *throttled.RateLimitResult
		for _, c := range clients {
			if c.lastUnreachableAt != nil && c.lastUnreachableAt.Add(globalConfiguration.UnreachableClientRetry).Before(time.Now()) {
				continue
			}

			limited, result, err := c.limiter.RateLimit(host.host, 0)
			if err != nil {
				log.Fatal(err)
			}

			if !limited && (bestResult == nil || result.Remaining > bestResult.Remaining) {
				bestResult = &result
				client = c
				continue
			}

			if limited && (retryAfter == nil || result.RetryAfter < *retryAfter) {
				retryAfter = &result.RetryAfter
				continue
			}
		}

		if client != nil {
			limited, _, err := client.limiter.RateLimit(host.host, 1)
			if err != nil {
				log.Fatal(err)
			}

			if limited {
				continue
			}
		}

		if retryAfter == nil {
			minDuration := 1 * time.Second
			retryAfter = &minDuration
		}

		return
	}
}

var clientAcquisitionPq sync.Map
var clientAcquisitionPqMutex sync.Mutex

type clientAcquisitionRequest struct {
	Callback chan *ProxyClient
	HostInfo *HostInfo
	Context  context.Context
	Priority float64
}

func runClientAcquisitionScheduler(ctx context.Context) {
	newClient := make(chan interface{})
	newClientBroadcast.Register(newClient)
	defer newClientBroadcast.Unregister(newClient)

	for {
		waitFor := 100 * time.Millisecond
		clientAcquisitionPq.Range(func(key, value interface{}) bool {
			priorityQueue := value.(pq.PriorityQueue)
			for {
				clientAcquisitionPqMutex.Lock()
				item, err := priorityQueue.Pop()
				clientAcquisitionPqMutex.Unlock()

				if err != nil {
					if err.Error() == "empty queue" {
						break
					}

					log.Fatal(err)
				}

				req := item.(clientAcquisitionRequest)

				if req.Context.Err() != nil {
					continue
				}

				client, retryAfter := tryGetClient(req.HostInfo)

				if client != nil {
					req.Callback <- client
				} else if *retryAfter < waitFor {
					waitFor = *retryAfter

					clientAcquisitionPqMutex.Lock()
					priorityQueue.Insert(req, 1000-req.Priority)
					clientAcquisitionPqMutex.Unlock()

					break
				}
			}

			return true
		})

		select {
		case <-ctx.Done():
			return
		case <-newClient:
		case <-time.After(waitFor):
			continue
		}
	}
}

func getClient(host *HostInfo, priority float64, ctx context.Context) (*ProxyClient, error) {
	cb := make(chan *ProxyClient)
	req := clientAcquisitionRequest{
		Callback: cb,
		HostInfo: host,
		Context:  ctx,
		Priority: priority,
	}

	clientAcquisitionPqMutex.Lock()
	queue, _ := clientAcquisitionPq.LoadOrStore(host.host, pq.New())
	priorityQueue := queue.(pq.PriorityQueue)
	priorityQueue.Insert(req, 1000-priority)
	clientAcquisitionPqMutex.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case client := <-cb:
			if client != nil {
				return client, nil
			}
		}
	}
}

func markClientUnreachable(client *ProxyClient) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	log.Printf("Marking client %s unrachable", client.id)
	now := time.Now()
	client.lastUnreachableAt = &now
}

func makeProxiedRequest(cfg RequestConfig, ctx context.Context) (*Response, error) {
	uri, err := url.Parse(cfg.Url)
	if err != nil {
		return nil, err
	}

	hostInfo := getHostInfo(uri.Hostname())
	if !hostInfo.isOnline() {
		return &Response{Status: ResponseStatusHostUnreachable}, nil
	}

	tries := 0
	var resp *Response
	for tries <= globalConfiguration.Retries {
		client, err := getClient(hostInfo, cfg.Priority, ctx)
		if err != nil {
			return nil, err
		}

		timeout := globalConfiguration.RequestTimeout
		if tries > 0 {
			timeout = globalConfiguration.RetryTimeout
		}

		resp, err = makeRequestWithClient(client, hostInfo, &cfg, ctx, timeout, cfg.Priority)
		if err != nil {
			return nil, err
		}

		if resp.Status == ResponseStatusTimeout || resp.Status == ResponseStatusHostUnreachable || (resp.Status == ResponseStatusOk && resp.Code == 502) {
			tries++
			continue
		} else if resp.Status == ResponseStatusProxyUnreachable {
			markClientUnreachable(client)
			continue
		} else if resp.Code == 429 {
			client.limiter.RateLimit(hostInfo.host, 100)
			tries++
			continue
		} else {
			break
		}
	}

	return resp, nil
}
