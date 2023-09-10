# Forward Proxy Manager

A proxy server that multiplexes your requests over a pool of proxies. It is made to easily scrape massive ammounts of pages on websites behind rate-limiters and other systems without expensive bot protection. Mainly Cloudflare on normal settings. It makes individual requests over random proxies from the pool, so it is best suited for unauthenticated sessions and public pages.

## Features

- Loads proxy list from an URL, checks proxies for availability
- Fakes user agent string and some headers to avoid bot detection
- Your script communicates with the proxy manager over a HTTPS PROXY protocol which makes it a breeze to use with any language/library HTTP client
- Supports HTTPS and HTTP2 on target server, tries to keep connections alive for maximal throughput
- Keeps track of rate limits on individual proxy-target pairs and backs off on 429 (Too Many Requests) errors
- Retries failed requests with a different proxy, up to a configured limit
- Forwards most headers
- Request priorities with `x-priority` header. Higher priority requests are processed first
- Has a request queue so you can send as many requests as you want without worry of overloading the proxy manager or triggering rate limits
- Optional web UI for monitoring and debugging pending requests

## Usage

![Usage](docs/usage.svg)

### 1. Run the Proxy Manager with Docker

```bash
docker run -it -p 8080:8080 -p 8081:8081 -e PROXY_LIST_URL=https://example.com/proxies ghcr.io/jkelin/forward-proxy-manager:latest
```

Or with docker-compose:

```yaml
version: "3.8"
services:
  proxy-manager:
    image: ghcr.io/jkelin/forward-proxy-manager:latest
    ports:
      - 8080:8080
      - 8081:8081
    environment:
      PROXY_LIST_URL: https://example.com/proxies
```

### 2. configure your HTTP client to use the proxy manager

Proxy Manager is a MITM proxy and uses self-signed certificate. You must configure your HTTP client to ignore certificate errors.

[Got](https://github.com/sindresorhus/got) in Node.js:

```typescript
import got from "got";
import { HttpsProxyAgent } from "hpagent";

const proxyAgent = new HttpsProxyAgent({
  keepAlive: true,
  proxy: "https://localhost:8080", // https needed to scrape https sites
  rejectUnauthorized: false, // ignore https errors (IMPORTANT)
});

const data = await got({
  method: "GET",
  url: "https://httpbin.org/get", // target url
  headers: {
    "x-priority": "0", // higher priority is going to be processed first
  },
  agent: {
    https: proxyAgent,
  },
}).json();
```

## Options

Configuration

Proxy Manager is configured with environment variables. You are only required to specify `PROXY_LIST_URL`.

Refer to [main.go](main.go) for definitions.

| Environment                 | Default      | Description                                                                                        |
| --------------------------- | ------------ | -------------------------------------------------------------------------------------------------- |
| `PROXY_LIST_URL`            | **REQUIRED** | URL from which to download the proxy list. Refer to proxy list format below                        |
| `REQUEST_TIMEOUT`           | `20s`        | Timeout for individual requests to target host                                                     |
| `RETRIES`                   | `1`          | Number of times to retry failed requests to target                                                 |
| `RETRY_TIMEOUT`             | `5s`         | Timeout subsequent requests to target                                                              |
| `INITIAL_IP_INFO_TIMEOUT`   | `10s`        | Timeout for proxy IP info request used to check proxy availability                                 |
| `HOST_INFO_REQUEST_TIMEOUT` | `5s`         | Timeout for host info request which we need to get information about HTTPS/HTTP2/IPV6 availability |
| `THROTTLE_REQUESTS_PER_MIN` | `30`         | Target host max requests per minute                                                                |
| `THROTTLE_REQUESTS_BURST`   | `5`          | Max concurrent target requests for a single proxy                                                  |
| `UNREACHABLE_CLIENT_RETRY`  | `60s`        | Retry for failing proxies                                                                          |
| `ENABLE_WEB`                | `false`      | Enable web UI on `:8080` for monitoring and debugging pending requests                             |

## Proxy list format

`HOST:PORT:USERNAME:PASSWORD`, newline separed.

Proxy list get downloaded from `PROXY_LIST_URL` on Proxy Manager's startup. Proxies must be SOCKS5, not HTTP. This format is what you get from [Webshare](https://www.webshare.io/?referral_code=x71lsv7e6k56) and similar services.

Example:

```
127.0.0.1:1080:username:password
127.0.0.2:1080:username:password
```
