package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/klauspost/compress/zstd"
)

// browserProfiles lists TLS/HTTP2 profiles whose fake headers getFakeHeaders can reproduce.
// Measured on tls.peet.ws against stock browsers:
//   - Chrome_144 TLS/HTTP2 equals real Chrome 149 (same JA4, akamai, shuffled extensions).
//   - Firefox_147 sends real Firefox 157's extension order; it still offers the ECDSA-CBC
//     ciphers and FFDHE groups that later Firefox dropped.
//
// Excluded: Firefox_148 offers DHE ciphers (0x33, 0x39) Firefox removed years ago;
// Firefox_135 omits session_ticket and psk_key_exchange_modes, which Firefox always sends.
var browserProfiles = []profiles.ClientProfile{
	profiles.Chrome_144,
	profiles.Chrome_146,
	profiles.Chrome_152,
	profiles.Firefox_147,
}

func randomBrowserProfile() profiles.ClientProfile {
	return browserProfiles[rand.IntN(len(browserProfiles))]
}

// shufflesTlsExtensions reports whether the real browser permutes ClientHello extensions on every connection.
// Chrome does since v110, so a fixed order (stable JA3) would be a bot tell; Firefox keeps a fixed order.
func shufflesTlsExtensions(profile profiles.ClientProfile) bool {
	return profile.GetClientHelloId().Client == "Chrome"
}

// chromeBrands reproduces Chromium's GenerateBrandVersionList: the GREASE brand, its version and the brand order
// are all derived from the major version, so sec-ch-ua is deterministic per release.
func chromeBrands(majorVersion int) string {
	greaseChars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	greaseVersions := []string{"8", "99", "24"}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}

	order := orders[majorVersion%len(orders)]
	brands := make([]string, 3)
	brands[order[0]] = fmt.Sprintf(`"Not%sA%sBrand";v="%s"`, greaseChars[majorVersion%len(greaseChars)], greaseChars[(majorVersion+1)%len(greaseChars)], greaseVersions[majorVersion%len(greaseVersions)])
	brands[order[1]] = fmt.Sprintf(`"Chromium";v="%d"`, majorVersion)
	brands[order[2]] = fmt.Sprintf(`"Google Chrome";v="%d"`, majorVersion)

	return strings.Join(brands, ", ")
}

// getFakeHeaders returns Windows desktop navigation headers in the order the profile's browser sends them.
// Panics for profiles without a known header template so a new profile cannot silently send mismatched headers.
func getFakeHeaders(profile profiles.ClientProfile) http.Header {
	identity := profile.GetClientHelloId()
	version := identity.Version

	switch identity.Client {
	case "Chrome":
		// PSK variants carry a suffix ("146_PSK"); only plain major versions map to a UA.
		majorVersion, err := strconv.Atoi(version)
		if err != nil {
			panic(fmt.Sprintf("unsupported Chrome profile version %s", version))
		}

		brands := chromeBrands(majorVersion)

		return http.Header{
			"sec-ch-ua":                 {brands},
			"sec-ch-ua-mobile":          {"?0"},
			"sec-ch-ua-platform":        {`"Windows"`},
			"upgrade-insecure-requests": {"1"},
			"user-agent":                {fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", version)},
			"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
			"sec-fetch-site":            {"none"},
			"sec-fetch-mode":            {"navigate"},
			"sec-fetch-user":            {"?1"},
			"sec-fetch-dest":            {"document"},
			"accept-encoding":           {"gzip, deflate, br, zstd"},
			"accept-language":           {"en-US,en;q=0.9"},
			"priority":                  {"u=0, i"},
			http.HeaderOrderKey: {
				"sec-ch-ua",
				"sec-ch-ua-mobile",
				"sec-ch-ua-platform",
				"upgrade-insecure-requests",
				"user-agent",
				"accept",
				"sec-fetch-site",
				"sec-fetch-mode",
				"sec-fetch-user",
				"sec-fetch-dest",
				"referer",
				"accept-encoding",
				"accept-language",
				"cookie",
				"priority",
			},
		}
	case "Firefox":
		return http.Header{
			"user-agent":                {fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%s.0) Gecko/20100101 Firefox/%s.0", version, version)},
			"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
			"accept-language":           {"en-US,en;q=0.5"},
			"accept-encoding":           {"gzip, deflate, br, zstd"},
			"upgrade-insecure-requests": {"1"},
			"sec-fetch-dest":            {"document"},
			"sec-fetch-mode":            {"navigate"},
			"sec-fetch-site":            {"none"},
			"sec-fetch-user":            {"?1"},
			"priority":                  {"u=0, i"},
			"te":                        {"trailers"},
			http.HeaderOrderKey: {
				"user-agent",
				"accept",
				"accept-language",
				"accept-encoding",
				"referer",
				"cookie",
				"upgrade-insecure-requests",
				"sec-fetch-dest",
				"sec-fetch-mode",
				"sec-fetch-site",
				"sec-fetch-user",
				"priority",
				"te",
			},
		}
	default:
		panic(fmt.Sprintf("unsupported browser profile %s", profile.GetClientHelloStr()))
	}
}

// decodeResponseBody removes every Content-Encoding layer. Encodings apply in listed order, so they are undone in reverse.
// Edge cases: empty and "identity" layers are no-ops; "deflate" means zlib-wrapped data per RFC 9110, raw DEFLATE fails;
// unknown encodings fail instead of returning bytes the client cannot interpret.
func decodeResponseBody(body io.Reader, contentEncoding string) ([]byte, error) {
	decoded, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}

	layers := strings.Split(contentEncoding, ",")
	for index := len(layers) - 1; index >= 0; index-- {
		encoding := strings.ToLower(strings.TrimSpace(layers[index]))
		if encoding == "" || encoding == "identity" {
			continue
		}

		decoded, err = decodeLayer(decoded, encoding)
		if err != nil {
			return nil, fmt.Errorf("decode %s body: %w", encoding, err)
		}
	}

	return decoded, nil
}

func decodeLayer(encoded []byte, encoding string) ([]byte, error) {
	reader := bytes.NewReader(encoded)

	switch encoding {
	case "gzip", "x-gzip":
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}

		defer gzipReader.Close()

		return io.ReadAll(gzipReader)
	case "deflate":
		zlibReader, err := zlib.NewReader(reader)
		if err != nil {
			return nil, err
		}

		defer zlibReader.Close()

		return io.ReadAll(zlibReader)
	case "br":
		return io.ReadAll(brotli.NewReader(reader))
	case "zstd":
		zstdReader, err := zstd.NewReader(reader)
		if err != nil {
			return nil, err
		}

		defer zstdReader.Close()

		return io.ReadAll(zstdReader)
	default:
		return nil, fmt.Errorf("unsupported content encoding")
	}
}
