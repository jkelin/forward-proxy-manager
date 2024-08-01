package main

import (
	"github.com/ReneKroon/ttlcache"
	"golang.org/x/net/http2"
	"net"
	"net/http"
	"strings"
	"time"
)

var hostCache = ttlcache.NewCache()

type HostInfo struct {
	host          string
	supportsHttp  bool
	supportsHttps bool
	supportsH2    bool
	supportsIPv4  bool
	supportsIPv6  bool
}

func fetchHostInfo(host string) *HostInfo {
	info := HostInfo{
		host: host,
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		info.supportsIPv4 = false
		info.supportsIPv6 = false

		return &info
	}

	for _, ip := range ips {
		if ip.To4() != nil {
			info.supportsIPv4 = true
		}

		if ip.To16() != nil {
			info.supportsIPv6 = true
		}
	}

	if !info.supportsIPv4 && !info.supportsIPv6 {
		return &info
	}

	client := &http.Client{Timeout: globalConfiguration.HostInfoRequestTimeout}
	resp, err := client.Get("http://" + host)
	info.supportsHttp = err == nil && resp.StatusCode != 426

	if err != nil {
		return &info
	}

	locationHeader := resp.Header.Get("Location")
	if strings.Contains(locationHeader, "https://") {
		info.supportsHttp = false
	}

	_, err = client.Get("https://" + host)
	info.supportsHttps = err == nil

	transport := &http2.Transport{}
	client = &http.Client{Transport: transport, Timeout: globalConfiguration.HostInfoRequestTimeout}
	h2Url := "https://" + host
	if !info.supportsHttps {
		h2Url = "http://" + host
	}

	_, err = client.Get(h2Url)
	info.supportsH2 = err == nil

	return &info
}

func (info HostInfo) isOnline() bool {
	return (info.supportsIPv4 || info.supportsIPv6) && (info.supportsHttp || info.supportsH2 || info.supportsHttps)
}

func getHostInfo(host string) *HostInfo {
	cachedInfo, exists := hostCache.Get(host)
	if !exists {
		info := fetchHostInfo(host)

		if info.isOnline() {
			hostCache.SetWithTTL(host, info, 60*time.Minute)
		} else {
			hostCache.SetWithTTL(host, info, 10*time.Second)
		}

		return info
	} else {
		return cachedInfo.(*HostInfo)
	}
}
