package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	http "github.com/bogdanfinn/fhttp"
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

			for _, name := range headers[http.HeaderOrderKey] {
				if headerValue(headers, name) == "" {
					t.Fatalf("ordered header absent: %s", name)
				}
			}

			if len(headers[http.HeaderOrderKey]) != len(headers)-1 {
				t.Fatal("header order does not cover all headers")
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

func TestDecodeResponseBody(t *testing.T) {
	payload := []byte("<html>server components — Xeon</html>")
	for _, encoding := range []string{"", "identity", "gzip", "deflate", "br", "zstd", "gzip, br"} {
		t.Run(encoding, func(t *testing.T) {
			encoded := payload
			for _, layer := range strings.Split(encoding, ",") {
				var buffer bytes.Buffer
				var writer io.WriteCloser
				switch strings.TrimSpace(layer) {
				case "", "identity":
					continue
				case "gzip":
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
				}

				if _, err := writer.Write(encoded); err != nil {
					t.Fatal(err)
				}

				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}

				encoded = buffer.Bytes()
			}

			decoded, err := decodeResponseBody(bytes.NewReader(encoded), encoding)
			if err != nil || !bytes.Equal(decoded, payload) {
				t.Fatalf("decoded %q, error %v", decoded, err)
			}
		})
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
