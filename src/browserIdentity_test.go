package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/andybalholm/brotli"
	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/klauspost/compress/zstd"
)

// headerValue reads the lowercase wire names getFakeHeaders stores without canonicalization.
func headerValue(headers http.Header, name string) string {
	return strings.Join(headers[name], ",")
}

func TestChromeBrands(t *testing.T) {
	// Values captured from real Chrome releases.
	for majorVersion, expected := range map[int]string{
		131: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		133: `"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"`,
		149: `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`,
	} {
		if actual := chromeBrands(majorVersion); actual != expected {
			t.Errorf("Chrome %d: got %s", majorVersion, actual)
		}
	}
}

func TestWithoutDefaultPort(t *testing.T) {
	for raw, expected := range map[string]string{
		"https://www.ebay.com:443/sch": "www.ebay.com",
		"http://www.ebay.com:80/":      "www.ebay.com",
		"https://www.ebay.com:8443/":   "www.ebay.com:8443",
		"http://www.ebay.com:443/":     "www.ebay.com:443",
		"https://www.ebay.com/":        "www.ebay.com",
		"https://[::1]:443/":           "[::1]",
	} {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}

		if actual := withoutDefaultPort(target); actual != expected {
			t.Errorf("%s: got %s", raw, actual)
		}
	}
}

func TestBrowserIdentity(t *testing.T) {
	for _, profile := range browserProfiles {
		t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
			headers := getFakeHeaders(profile)
			identity := profile.GetClientHelloId()
			if !strings.Contains(headerValue(headers, "user-agent"), identity.Client+"/"+identity.Version+".") {
				t.Fatalf("UA does not match profile: %s", headerValue(headers, "user-agent"))
			}

			if headerValue(headers, "accept-encoding") != "gzip, deflate, br, zstd" {
				t.Fatal("missing browser compression support")
			}

			// referer and cookie are ordered but only present on session navigations.
			optional := map[string]bool{"referer": true, "cookie": true}
			ordered := map[string]bool{}
			for _, name := range headers[http.HeaderOrderKey] {
				ordered[name] = true
				if headerValue(headers, name) == "" && !optional[name] {
					t.Fatalf("ordered header absent: %s", name)
				}
			}

			for name := range headers {
				if name != http.HeaderOrderKey && !ordered[name] {
					t.Fatalf("header missing from order: %s", name)
				}
			}

			if identity.Client == "Chrome" {
				if !strings.Contains(headerValue(headers, "sec-ch-ua"), `"Google Chrome";v="`+identity.Version+`"`) {
					t.Fatal("Chrome client hint version differs")
				}

				if !reflect.DeepEqual(profile.GetPseudoHeaderOrder(), []string{":method", ":authority", ":scheme", ":path"}) {
					t.Fatal("unexpected Chrome pseudo-header order")
				}
			} else if headerValue(headers, "sec-ch-ua") != "" || headerValue(headers, "sec-ch-ua-platform") != "" {
				t.Fatal("Firefox must not send Chromium client hints")
			}
		})
	}
}

func TestBrowserUserAgents(t *testing.T) {
	for _, profile := range browserProfiles {
		t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
			identity := profile.GetClientHelloId()
			expected := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + identity.Version + ".0.0.0 Safari/537.36"
			if identity.Client == "Firefox" {
				expected = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:" + identity.Version + ".0) Gecko/20100101 Firefox/" + identity.Version + ".0"
			}

			if actual := headerValue(getFakeHeaders(profile), "user-agent"); actual != expected {
				t.Fatalf("user-agent %q, want %q", actual, expected)
			}
		})
	}
}

func TestBrowserHeaderOrderIsUnique(t *testing.T) {
	for _, profile := range browserProfiles {
		order := getFakeHeaders(profile)[http.HeaderOrderKey]
		seen := map[string]bool{}
		for _, name := range order {
			if seen[name] || name != strings.ToLower(name) {
				t.Errorf("%s: duplicate or non-lowercase ordered header %q", profile.GetClientHelloStr(), name)
			}

			seen[name] = true
		}
	}
}

func TestChromeBrandsMatchProfiles(t *testing.T) {
	for _, profile := range browserProfiles {
		identity := profile.GetClientHelloId()
		if identity.Client != "Chrome" {
			continue
		}

		var majorVersion int
		if _, err := fmt.Sscan(identity.Version, &majorVersion); err != nil {
			t.Fatal(err)
		}

		if actual := headerValue(getFakeHeaders(profile), "sec-ch-ua"); actual != chromeBrands(majorVersion) {
			t.Errorf("%s: sec-ch-ua %q, want %q", profile.GetClientHelloStr(), actual, chromeBrands(majorVersion))
		}
	}
}

// Every call returns a fresh header map: a request editing its copy must not change the next one.
func TestFakeHeadersIsolation(t *testing.T) {
	for _, profile := range browserProfiles {
		t.Run(profile.GetClientHelloStr(), func(t *testing.T) {
			baseline := getFakeHeaders(profile)

			mutated := getFakeHeaders(profile)
			mutated["user-agent"][0] = "bot"
			mutated["accept-encoding"][0] = "identity"
			mutated[http.HeaderOrderKey][0] = "x-injected"
			mutated["referer"] = []string{"https://evil.example/"}
			delete(mutated, "accept")

			if again := getFakeHeaders(profile); !reflect.DeepEqual(again, baseline) {
				t.Fatalf("header edits leaked into later identities: %v", again)
			}
		})
	}
}

func TestBrowserProfileSet(t *testing.T) {
	names := map[string]bool{}
	for _, profile := range browserProfiles {
		name := profile.GetClientHelloStr()
		if names[name] {
			t.Fatalf("duplicate profile %s", name)
		}

		names[name] = true
	}

	// Excluded on purpose: their fingerprints do not match any real browser (see browserProfiles).
	for _, excluded := range []profiles.ClientProfile{profiles.Firefox_148, profiles.Firefox_135} {
		if names[excluded.GetClientHelloStr()] {
			t.Errorf("excluded profile %s is selectable", excluded.GetClientHelloStr())
		}
	}
}

func TestRandomBrowserProfile(t *testing.T) {
	names := make([]string, 0, len(browserProfiles))
	for _, profile := range browserProfiles {
		names = append(names, profile.GetClientHelloStr())
	}

	// 1000 uniform draws miss one of a handful of profiles with negligible probability.
	seen := map[string]bool{}
	for range 1000 {
		name := randomBrowserProfile().GetClientHelloStr()
		if !slices.Contains(names, name) {
			t.Fatalf("random profile %s is not in browserProfiles", name)
		}

		seen[name] = true
	}

	if len(seen) != len(names) {
		t.Fatalf("random selection covered %d of %d profiles", len(seen), len(names))
	}
}

func TestShufflesTlsExtensions(t *testing.T) {
	for _, profile := range browserProfiles {
		expected := profile.GetClientHelloId().Client == "Chrome"
		if actual := shufflesTlsExtensions(profile); actual != expected {
			t.Errorf("%s: shufflesTlsExtensions = %t", profile.GetClientHelloStr(), actual)
		}
	}

	if shufflesTlsExtensions(profiles.Firefox_147) || !shufflesTlsExtensions(profiles.Chrome_144) {
		t.Fatal("Chrome must shuffle extensions and Firefox must not")
	}
}

func TestFakeHeadersRejectUnsupportedProfile(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "unsupported browser profile") {
			t.Fatalf("expected unsupported profile panic, got %v", recovered)
		}
	}()

	getFakeHeaders(profiles.Safari_16_0)
}

// encodeBody applies Content-Encoding layers in listed order, as a server would.
// Edge cases: "" and "identity" layers are skipped; "deflate" is zlib-wrapped per RFC 9110; layer names ignore case.
func encodeBody(t *testing.T, payload []byte, encoding string) []byte {
	t.Helper()

	encoded := payload
	for _, layer := range strings.Split(encoding, ",") {
		var buffer bytes.Buffer
		var writer io.WriteCloser
		switch strings.ToLower(strings.TrimSpace(layer)) {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			writer = gzip.NewWriter(&buffer)
		case "deflate":
			writer = zlib.NewWriter(&buffer)
		case "br":
			writer = brotli.NewWriter(&buffer)
		case "zstd":
			var err error
			writer, err = zstd.NewWriter(&buffer)
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("encodeBody: unsupported layer %q", layer)
		}

		if _, err := writer.Write(encoded); err != nil {
			t.Fatal(err)
		}

		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		encoded = buffer.Bytes()
	}

	return encoded
}

func TestDecodeResponseBody(t *testing.T) {
	payload := []byte("<html>server components — Xeon</html>")
	for _, encoding := range []string{
		"", "identity", "gzip", "deflate", "br", "zstd", "gzip, br",
		"x-gzip", "GZIP", " br ,  gzip ", "gzip,,br", "identity, zstd, identity",
		"gzip, gzip", "deflate, gzip, br, zstd",
	} {
		t.Run(encoding, func(t *testing.T) {
			decoded, err := decodeResponseBody(bytes.NewReader(encodeBody(t, payload, encoding)), encoding)
			if err != nil || !bytes.Equal(decoded, payload) {
				t.Fatalf("decoded %q, error %v", decoded, err)
			}
		})
	}

	if decoded, err := decodeResponseBody(bytes.NewReader(nil), ""); err != nil || len(decoded) != 0 {
		t.Fatalf("empty identity body: %q, %v", decoded, err)
	}
}

func TestDecodeResponseBodyErrors(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "br", "zstd", "unknown"} {
		t.Run(encoding, func(t *testing.T) {
			if _, err := decodeResponseBody(strings.NewReader("not compressed"), encoding); err == nil {
				t.Fatal("invalid encoding or body must fail")
			}
		})
	}
}

func TestDecodeResponseBodyCorruption(t *testing.T) {
	payload := []byte(strings.Repeat("<html>server components — Xeon</html>", 32))
	gzipped := encodeBody(t, payload, "gzip")
	zlibbed := encodeBody(t, payload, "deflate")
	brotlied := encodeBody(t, payload, "br")
	zstded := encodeBody(t, payload, "zstd")
	stacked := encodeBody(t, payload, "gzip, br")

	var rawDeflate bytes.Buffer
	deflateWriter, err := flate.NewWriter(&rawDeflate, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := deflateWriter.Write(payload); err != nil {
		t.Fatal(err)
	}

	if err := deflateWriter.Close(); err != nil {
		t.Fatal(err)
	}

	// gzip ends with CRC-32 then size; zlib ends with Adler-32.
	badGzipChecksum := bytes.Clone(gzipped)
	badGzipChecksum[len(badGzipChecksum)-8] ^= 0xff
	badZlibChecksum := bytes.Clone(zlibbed)
	badZlibChecksum[len(badZlibChecksum)-1] ^= 0xff

	for _, testCase := range []struct {
		name     string
		body     []byte
		encoding string
		cause    error
	}{
		{"truncated gzip", gzipped[:len(gzipped)-4], "gzip", io.ErrUnexpectedEOF},
		{"gzip checksum", badGzipChecksum, "gzip", gzip.ErrChecksum},
		{"zlib checksum", badZlibChecksum, "deflate", zlib.ErrChecksum},
		{"raw deflate", rawDeflate.Bytes(), "deflate", nil},
		{"truncated br", brotlied[:len(brotlied)/2], "br", nil},
		{"truncated zstd", zstded[:len(zstded)-3], "zstd", nil},
		{"empty gzip", nil, "gzip", nil},
		{"wrong layer order", stacked, "br, gzip", nil},
		{"unknown outer layer", gzipped, "gzip, compress", nil},
		{"unknown inner layer", gzipped, "compress, gzip", nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decoded, err := decodeResponseBody(bytes.NewReader(testCase.body), testCase.encoding)
			if err == nil {
				t.Fatalf("corrupt body decoded to %q", decoded)
			}

			if decoded != nil || !strings.HasPrefix(err.Error(), "decode ") {
				t.Fatalf("decoded %q, error %v", decoded, err)
			}

			if testCase.cause != nil && !errors.Is(err, testCase.cause) {
				t.Fatalf("error %v does not wrap %v", err, testCase.cause)
			}
		})
	}
}

func TestDecodeResponseBodyReadError(t *testing.T) {
	cause := errors.New("connection reset")
	if decoded, err := decodeResponseBody(iotest.ErrReader(cause), "gzip"); !errors.Is(err, cause) || decoded != nil {
		t.Fatalf("decoded %q, error %v", decoded, err)
	}
}
