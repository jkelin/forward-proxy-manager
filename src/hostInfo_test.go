package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReneKroon/ttlcache"
)

// probeResult is the fake outcome for one URL scheme.
type probeResult struct {
	status   int
	location string
	err      error
}

// trackedBody records whether the code under test closed a response body.
type trackedBody struct {
	io.Reader
	closed bool
}

func (body *trackedBody) Close() error {
	body.closed = true

	return nil
}

// fakeProbeTransport answers by scheme without touching the network.
type fakeProbeTransport struct {
	results map[string]probeResult
	calls   map[string]int
	bodies  []*trackedBody
}

func (transport *fakeProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls[request.URL.Scheme]++

	result, ok := transport.results[request.URL.Scheme]
	if !ok {
		return nil, errors.New("unexpected scheme " + request.URL.Scheme)
	}

	if result.err != nil {
		return nil, result.err
	}

	body := &trackedBody{Reader: strings.NewReader("body")}
	transport.bodies = append(transport.bodies, body)

	header := http.Header{}
	if result.location != "" {
		header.Set("Location", result.location)
	}

	return &http.Response{
		StatusCode: result.status,
		Header:     header,
		Body:       body,
		Request:    request,
	}, nil
}

func newFakeProbe(ips []net.IP, lookupErr error, results map[string]probeResult) (hostInfoProbe, *fakeProbeTransport) {
	transport := &fakeProbeTransport{results: results, calls: map[string]int{}}
	probe := hostInfoProbe{
		lookupIP: func(string) ([]net.IP, error) { return ips, lookupErr },
		client:   &http.Client{Transport: transport},
	}

	return probe, transport
}

var (
	bothProbesOk = map[string]probeResult{
		"http":  {status: http.StatusOK},
		"https": {status: http.StatusOK},
	}
	ipv4Address       = net.ParseIP("192.0.2.10")
	ipv4MappedAddress = net.IPv4(198, 51, 100, 7)
	ipv6Address       = net.ParseIP("2001:db8::1")
	loopbackIPv6      = net.ParseIP("::1")
)

func TestHostInfoAddressFamilies(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		ips          []net.IP
		lookupErr    error
		expectedIPv4 bool
		expectedIPv6 bool
		expectedHttp bool
	}{
		{"ipv4 only", []net.IP{ipv4Address}, nil, true, false, true},
		{"ipv4 mapped in 16 bytes", []net.IP{ipv4MappedAddress}, nil, true, false, true},
		{"ipv6 only", []net.IP{ipv6Address}, nil, false, true, true},
		{"loopback ipv6", []net.IP{loopbackIPv6}, nil, false, true, true},
		{"dual stack", []net.IP{ipv4Address, ipv6Address}, nil, true, true, true},
		{"malformed address ignored", []net.IP{{1, 2, 3}}, nil, false, false, false},
		{"no addresses", nil, nil, false, false, false},
		{"lookup failure", []net.IP{ipv4Address}, errors.New("no such host"), false, false, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			probe, transport := newFakeProbe(testCase.ips, testCase.lookupErr, bothProbesOk)
			info := probe.fetch("example.test")

			if info.host != "example.test" {
				t.Fatalf("host = %q", info.host)
			}

			if info.supportsIPv4 != testCase.expectedIPv4 || info.supportsIPv6 != testCase.expectedIPv6 {
				t.Fatalf("ipv4/ipv6 = %v/%v, want %v/%v", info.supportsIPv4, info.supportsIPv6, testCase.expectedIPv4, testCase.expectedIPv6)
			}

			if info.supportsHttp != testCase.expectedHttp {
				t.Fatalf("supportsHttp = %v, want %v", info.supportsHttp, testCase.expectedHttp)
			}

			if !testCase.expectedHttp && len(transport.calls) != 0 {
				t.Fatalf("unreachable host was probed: %v", transport.calls)
			}
		})
	}
}

func TestHostInfoProbes(t *testing.T) {
	transportFailure := errors.New("connection refused")

	for _, testCase := range []struct {
		name               string
		http               probeResult
		https              probeResult
		expectedHttp       bool
		expectedHttps      bool
		expectedHttpsCalls int
	}{
		{"both succeed", probeResult{status: http.StatusOK}, probeResult{status: http.StatusOK}, true, true, 1},
		{"error status still answers", probeResult{status: http.StatusInternalServerError}, probeResult{status: http.StatusNotFound}, true, true, 1},
		{"upgrade required", probeResult{status: http.StatusUpgradeRequired}, probeResult{status: http.StatusOK}, false, true, 1},
		{"https location header", probeResult{status: http.StatusMultipleChoices, location: "https://example.test/"}, probeResult{status: http.StatusOK}, false, true, 1},
		{"http location header", probeResult{status: http.StatusMultipleChoices, location: "http://example.test/other"}, probeResult{status: http.StatusOK}, true, true, 1},
		{"https transport failure", probeResult{status: http.StatusOK}, probeResult{err: transportFailure}, true, false, 1},
		{"http transport failure skips https", probeResult{err: transportFailure}, probeResult{status: http.StatusOK}, false, false, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			probe, transport := newFakeProbe([]net.IP{ipv4Address}, nil, map[string]probeResult{
				"http":  testCase.http,
				"https": testCase.https,
			})
			info := probe.fetch("example.test")

			if info.supportsHttp != testCase.expectedHttp || info.supportsHttps != testCase.expectedHttps {
				t.Fatalf("http/https = %v/%v, want %v/%v", info.supportsHttp, info.supportsHttps, testCase.expectedHttp, testCase.expectedHttps)
			}

			if transport.calls["http"] != 1 || transport.calls["https"] != testCase.expectedHttpsCalls {
				t.Fatalf("probe calls = %v", transport.calls)
			}

			for index, body := range transport.bodies {
				if !body.closed {
					t.Fatalf("response body %d leaked", index)
				}
			}
		})
	}
}

func TestHostInfoIsOnline(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		info     HostInfo
		expected bool
	}{
		{"nothing", HostInfo{}, false},
		{"address without protocol", HostInfo{supportsIPv4: true, supportsIPv6: true}, false},
		{"protocol without address", HostInfo{supportsHttp: true, supportsHttps: true}, false},
		{"ipv4 http", HostInfo{supportsIPv4: true, supportsHttp: true}, true},
		{"ipv6 https", HostInfo{supportsIPv6: true, supportsHttps: true}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if online := testCase.info.isOnline(); online != testCase.expected {
				t.Fatalf("isOnline = %v, want %v", online, testCase.expected)
			}
		})
	}
}

func TestHostInfoTTL(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		info     HostInfo
		expected time.Duration
	}{
		{"online", HostInfo{supportsIPv4: true, supportsHttp: true}, onlineHostInfoTTL},
		{"offline", HostInfo{supportsIPv4: true}, offlineHostInfoTTL},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if ttl := hostInfoTTL(testCase.info); ttl != testCase.expected {
				t.Fatalf("ttl = %v, want %v", ttl, testCase.expected)
			}
		})
	}
}

func TestHostInfoCache(t *testing.T) {
	for _, testCase := range []struct {
		name string
		info HostInfo
	}{
		{"online result", HostInfo{supportsIPv4: true, supportsHttps: true}},
		{"offline result", HostInfo{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cache := ttlcache.NewCache()
			t.Cleanup(cache.Close)

			var lock sync.Mutex
			fetches := map[string]int{}
			fetch := func(host string) *HostInfo {
				lock.Lock()
				defer lock.Unlock()

				fetches[host]++
				info := testCase.info
				info.host = host

				return &info
			}

			first := getCachedHostInfo(cache, "a.test", fetch)
			second := getCachedHostInfo(cache, "a.test", fetch)
			other := getCachedHostInfo(cache, "b.test", fetch)

			if first != second {
				t.Fatal("cached lookup returned a different HostInfo")
			}

			if other.host != "b.test" || fetches["a.test"] != 1 || fetches["b.test"] != 1 {
				t.Fatalf("unexpected fetches %v for %q", fetches, other.host)
			}
		})
	}
}
