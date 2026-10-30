package main

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestHostInfoFor(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		raw      string
		expected string
	}{
		{"https", "https://www.ebay.com/sch/i.html?_nkw=x", "www.ebay.com"},
		{"http", "http://a.test/", "a.test"},
		{"explicit port", "https://a.test:8443/x", "a.test"},
		{"ipv6 literal", "https://[2001:db8::1]:443/", "2001:db8::1"},
		{"unresolvable host still accepted", "https://down.invalid/", "down.invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			info, err := hostInfoFor(mustParseUrl(t, testCase.raw))
			if err != nil {
				t.Fatal(err)
			}

			if info.host != testCase.expected {
				t.Fatalf("host = %q, want %q", info.host, testCase.expected)
			}
		})
	}
}

func TestHostInfoForRejectsInvalidTargets(t *testing.T) {
	for _, testCase := range []struct {
		name string
		uri  *url.URL
	}{
		{"ftp", &url.URL{Scheme: "ftp", Host: "a.test"}},
		{"no scheme", &url.URL{Host: "a.test"}},
		{"no host", &url.URL{Scheme: "https", Path: "/x"}},
		{"port only", &url.URL{Scheme: "https", Host: ":443"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := hostInfoFor(testCase.uri); !errors.Is(err, errInvalidTarget) {
				t.Fatalf("got %v, want errInvalidTarget", err)
			}
		})
	}
}

func TestHostInfoForRedactsCredentials(t *testing.T) {
	_, err := hostInfoFor(&url.URL{Scheme: "ftp", User: url.UserPassword("user", "secret"), Host: "a.test"})
	if !errors.Is(err, errInvalidTarget) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("got %v, want errInvalidTarget without the password", err)
	}
}
