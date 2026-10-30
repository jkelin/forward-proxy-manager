package main

import (
	"errors"
	"fmt"
	"net/url"
)

// errInvalidTarget means a request URL is not an absolute http(s) URL, so no proxy is tried.
var errInvalidTarget = errors.New("target is not an absolute http(s) URL")

// HostInfo identifies a request's target host; rate limits and back-offs are kept per host.
type HostInfo struct {
	host string
}

// hostInfoFor derives HostInfo from the request URL alone. FPM never contacts a target directly (no DNS lookup,
// no reachability probe): every byte to a target leaves through a proxy exit, so the FPM machine's IP stays hidden.
// Unreachable targets surface as HostUnreachable or Timeout responses from the exits instead.
//
// Edge cases: schemes other than http and https, and URLs without a hostname, return errInvalidTarget.
func hostInfoFor(uri *url.URL) (HostInfo, error) {
	if (uri.Scheme != "http" && uri.Scheme != "https") || uri.Hostname() == "" {
		return HostInfo{}, fmt.Errorf("%w: %q", errInvalidTarget, uri.Redacted())
	}

	return HostInfo{host: uri.Hostname()}, nil
}
