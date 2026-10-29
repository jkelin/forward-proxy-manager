package main

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// TestMain silences the service's request logging so failures stay readable.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// testLimits never limits a test exit unless a test asks for it.
func testLimits() exitLimits {
	return exitLimits{
		requestsPerMin:     60000,
		burst:              1000,
		unreachableRetry:   time.Minute,
		rateLimitedBackoff: time.Minute,
		clock:              time.Now,
	}
}

// onlineHost reports every host as reachable; https decides the scheme requests are sent with.
func onlineHost(https bool) func(host string) *HostInfo {
	return func(host string) *HostInfo {
		return &HostInfo{host: host, supportsHttp: !https, supportsHttps: https, supportsIPv4: true}
	}
}

func offlineHost(host string) *HostInfo {
	return &HostInfo{host: host}
}

// SOCKS5 reply codes (RFC 1928).
const (
	socksSucceeded          byte = 0x00
	socksGeneralFailure     byte = 0x01
	socksHostUnreachable    byte = 0x04
	socksConnectionRefused  byte = 0x05
	socksCommandUnsupported byte = 0x07
)

// socksServer is a minimal in-process SOCKS5 proxy (CONNECT only, optional username/password auth).
type socksServer struct {
	listener net.Listener
	username string
	password string
	// forcedReply, when non-zero, answers every CONNECT with that failure code instead of dialing.
	forcedReply atomic.Uint32
	connects    atomic.Int64
	targetsLock sync.Mutex
	targets     []string
}

func startSocksServer(t *testing.T, username string, password string) *socksServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &socksServer{listener: listener, username: username, password: password}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go server.serve(conn)
		}
	}()

	return server
}

func (server *socksServer) config() ProxyConfig {
	address := server.listener.Addr().(*net.TCPAddr)

	return ProxyConfig{host: address.IP.String(), port: int64(address.Port), username: server.username, password: server.password}
}

func (server *socksServer) proxyUrl(password string) string {
	config := server.config()

	return "socks5h://" + config.username + ":" + password + "@" + net.JoinHostPort(config.host, strconv.FormatInt(config.port, 10))
}

func (server *socksServer) connectedTargets() []string {
	server.targetsLock.Lock()
	defer server.targetsLock.Unlock()

	return append([]string(nil), server.targets...)
}

func (server *socksServer) serve(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}

	methods := make([]byte, header[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	wanted := byte(0x00)
	if server.username != "" {
		wanted = 0x02
	}

	offered := false
	for _, method := range methods {
		offered = offered || method == wanted
	}

	if !offered {
		_, _ = conn.Write([]byte{5, 0xFF})
		return
	}

	_, _ = conn.Write([]byte{5, wanted})

	if wanted == 0x02 && !server.authenticate(conn) {
		return
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}

	target, ok := readSocksAddress(conn, request[3])
	if !ok {
		return
	}

	server.connects.Add(1)
	server.targetsLock.Lock()
	server.targets = append(server.targets, target)
	server.targetsLock.Unlock()

	if request[1] != 0x01 {
		writeSocksReply(conn, socksCommandUnsupported)
		return
	}

	if forced := byte(server.forcedReply.Load()); forced != socksSucceeded {
		writeSocksReply(conn, forced)
		return
	}

	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		writeSocksReply(conn, socksConnectionRefused)
		return
	}
	defer upstream.Close()

	writeSocksReply(conn, socksSucceeded)
	_ = conn.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		done <- struct{}{}
	}()
	<-done
}

func (server *socksServer) authenticate(conn net.Conn) bool {
	version := make([]byte, 2)
	if _, err := io.ReadFull(conn, version); err != nil {
		return false
	}

	username := make([]byte, version[1])
	if _, err := io.ReadFull(conn, username); err != nil {
		return false
	}

	passwordLength := make([]byte, 1)
	if _, err := io.ReadFull(conn, passwordLength); err != nil {
		return false
	}

	password := make([]byte, passwordLength[0])
	if _, err := io.ReadFull(conn, password); err != nil {
		return false
	}

	if string(username) != server.username || string(password) != server.password {
		_, _ = conn.Write([]byte{1, 1})
		return false
	}

	_, _ = conn.Write([]byte{1, 0})

	return true
}

func readSocksAddress(conn net.Conn, addressType byte) (string, bool) {
	var host string

	switch addressType {
	case 0x01, 0x04:
		size := 4
		if addressType == 0x04 {
			size = 16
		}

		address := make([]byte, size)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", false
		}

		host = net.IP(address).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", false
		}

		name := make([]byte, length[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", false
		}

		host = string(name)
	default:
		return "", false
	}

	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", false
	}

	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), true
}

func writeSocksReply(conn net.Conn, code byte) {
	_, _ = conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}

// startTargetServer serves handler over TLS with HTTP/2 enabled, like a real site.
func startTargetServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	return server
}

// startPlainServer serves handler over plain HTTP/1.1.
func startPlainServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}

// newSocksProxyClient builds an exit routed through socks that trusts the test servers' certificates.
func newSocksProxyClient(t *testing.T, socks *socksServer, limits exitLimits) *ProxyClient {
	t.Helper()

	return newSocksProxyClientWithPassword(t, socks, socks.password, limits)
}

// newSocksProxyClientWithPassword is newSocksProxyClient authenticating with password instead of the server's.
func newSocksProxyClientWithPassword(t *testing.T, socks *socksServer, password string, limits exitLimits) *ProxyClient {
	t.Helper()

	client, err := newProxyClient(socks.config().host, profiles.Chrome_144, limits,
		tls_client.WithProxyUrl(socks.proxyUrl(password)),
		tls_client.WithInsecureSkipVerify(),
	)
	if err != nil {
		t.Fatal(err)
	}

	return client
}

// startScheduler runs a scheduler with the given exits until the test ends.
func startScheduler(t *testing.T, scheduler *RequestScheduler, proxies ...proxyExit) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := scheduler.start(ctx)
	scheduler.setProxies(proxies)

	t.Cleanup(func() {
		cancel()
		<-stopped
	})
}

// closedPort returns a loopback address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	_ = listener.Close()

	return address
}
