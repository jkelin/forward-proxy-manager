package main

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ReneKroon/ttlcache"
)

const (
	onlineHostInfoTTL  = 60 * time.Minute
	offlineHostInfoTTL = 10 * time.Second
)

var hostCache = ttlcache.NewCache()

type HostInfo struct {
	host          string
	supportsHttp  bool
	supportsHttps bool
	supportsIPv4  bool
	supportsIPv6  bool
}

// hostInfoProbe holds the network dependencies used to classify a host.
type hostInfoProbe struct {
	lookupIP func(host string) ([]net.IP, error)
	client   *http.Client
}

func fetchHostInfo(host string) *HostInfo {
	probe := hostInfoProbe{
		lookupIP: net.LookupIP,
		client:   &http.Client{Timeout: globalConfiguration.HostInfoRequestTimeout},
	}

	return probe.fetch(host)
}

// fetch resolves host, then probes plain HTTP and HTTPS.
//
// Edge cases:
//   - DNS failure, no addresses or only malformed addresses skip both HTTP probes.
//   - IPv4-mapped IPv6 addresses count as IPv4 only.
//   - Redirects are followed; the final response decides plain HTTP support.
//   - Status 426 or a final https:// Location marks plain HTTP unsupported.
//   - A plain HTTP transport failure skips the HTTPS probe.
//   - Any HTTPS response, including error statuses, counts as HTTPS support.
//   - Every response body is closed; bodies are not read.
func (probe hostInfoProbe) fetch(host string) *HostInfo {
	info := HostInfo{
		host: host,
	}

	ips, err := probe.lookupIP(host)
	if err != nil {
		return &info
	}

	for _, ip := range ips {
		if ip.To4() != nil {
			info.supportsIPv4 = true
		} else if ip.To16() != nil {
			info.supportsIPv6 = true
		}
	}

	if !info.supportsIPv4 && !info.supportsIPv6 {
		return &info
	}

	resp, err := probe.client.Get("http://" + host)
	if err != nil {
		return &info
	}

	info.supportsHttp = resp.StatusCode != http.StatusUpgradeRequired &&
		!strings.Contains(resp.Header.Get("Location"), "https://")
	_ = resp.Body.Close()

	resp, err = probe.client.Get("https://" + host)
	if err != nil {
		return &info
	}

	info.supportsHttps = true
	_ = resp.Body.Close()

	return &info
}

func (info HostInfo) isOnline() bool {
	return (info.supportsIPv4 || info.supportsIPv6) && (info.supportsHttp || info.supportsHttps)
}

// hostInfoTTL keeps online hosts long and retries offline hosts soon.
func hostInfoTTL(info HostInfo) time.Duration {
	if info.isOnline() {
		return onlineHostInfoTTL
	}

	return offlineHostInfoTTL
}

func getHostInfo(host string) *HostInfo {
	return getCachedHostInfo(hostCache, host, fetchHostInfo)
}

// getCachedHostInfo returns the cached HostInfo for host or fetches and caches it.
//
// Edge cases: concurrent misses for one host each fetch, and the last result wins;
// a cache hit extends the entry TTL (ttlcache default).
func getCachedHostInfo(cache *ttlcache.Cache, host string, fetch func(host string) *HostInfo) *HostInfo {
	if cachedInfo, exists := cache.Get(host); exists {
		return cachedInfo.(*HostInfo)
	}

	info := fetch(host)
	cache.SetWithTTL(host, info, hostInfoTTL(*info))

	return info
}
