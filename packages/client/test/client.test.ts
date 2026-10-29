import {
  Server,
  ServerCredentials,
  status,
  type handleUnaryCall,
  type ServerUnaryCall,
  type sendUnaryData,
} from "@grpc/grpc-js";
import { afterEach, describe, expect, test } from "bun:test";
import { z, ZodError } from "zod";

import { ProxyService, type ProxyRequest, type ProxyResponse } from "../src/generated/service.js";
import {
  ForwardProxyConnectionError,
  ForwardProxyError,
  ForwardProxyManager,
  ProxyErrorType,
  type WarmupStatus,
} from "../src/index.js";

type Call = ServerUnaryCall<ProxyRequest, ProxyResponse>;

interface FakeServer {
  manager: ForwardProxyManager;
  calls: ProxyRequest[];
  /** Resolves once the server has received `count` calls. */
  waitForCalls: (count: number) => Promise<void>;
}

const servers: Server[] = [];
const managers: ForwardProxyManager[] = [];

function success(
  statusCode: number,
  body: string,
  headers: Record<string, string> = {},
): ProxyResponse {
  return {
    response: { $case: "success", value: { status: statusCode, headers, body: Buffer.from(body) } },
  };
}

// Bun's `expect(promise).rejects` stalls gRPC I/O while it waits, so rejections are awaited first.
async function rejection<T>(promise: Promise<T>): Promise<Error> {
  try {
    await promise;
  } catch (error) {
    return error instanceof Error ? error : new Error(String(error));
  }

  throw new Error("Expected the promise to reject");
}

function serviceError(code: status, details: string): Error {
  return Object.assign(new Error(details), { code, details });
}

async function startServer(
  handler: (call: Call, callback: sendUnaryData<ProxyResponse>) => void,
  options: ConstructorParameters<typeof ForwardProxyManager>[1] = {},
): Promise<FakeServer> {
  const calls: ProxyRequest[] = [];
  const waiters: { count: number; resolve: () => void }[] = [];
  const server = new Server();

  const sendRequest: handleUnaryCall<ProxyRequest, ProxyResponse> = (call, callback) => {
    calls.push(call.request);
    handler(call, callback);

    for (const waiter of waiters.filter((entry) => entry.count <= calls.length)) {
      waiters.splice(waiters.indexOf(waiter), 1);
      waiter.resolve();
    }
  };

  const waitForCalls = async (count: number): Promise<void> => {
    if (calls.length >= count) {
      return;
    }

    await new Promise<void>((resolve) => {
      waiters.push({ count, resolve });
    });
  };

  server.addService(ProxyService, { sendRequest });

  const port = await new Promise<number>((resolve, reject) => {
    server.bindAsync("127.0.0.1:0", ServerCredentials.createInsecure(), (error, boundPort) => {
      if (error) {
        reject(error);
      } else {
        resolve(boundPort);
      }
    });
  });

  const manager = new ForwardProxyManager(`127.0.0.1:${port}`, { logger: null, ...options });

  servers.push(server);
  managers.push(manager);

  return { manager, calls, waitForCalls };
}

afterEach(() => {
  for (const manager of managers.splice(0)) {
    manager.close();
  }

  for (const server of servers.splice(0)) {
    server.forceShutdown();
  }
});

describe("request mapping", () => {
  test("sends url, priority, retry codes and warmup url; returns lower-cased headers", async () => {
    const { manager, calls } = await startServer((_call, callback) => {
      callback(
        null,
        success(200, "hello", { "Content-Type": "text/plain", "X-Fpm-Warmup": "fresh" }),
      );
    });

    const result = await manager.proxyRequest("https://example.com/page", {
      priority: 5,
      retryOnCodes: [403, 503],
      warmupUrl: "https://example.com/",
    });

    expect(calls).toEqual([
      {
        url: "https://example.com/page",
        method: "",
        headers: {},
        priority: 5,
        retryOnCodes: [403, 503],
        warmupUrl: "https://example.com/",
      },
    ]);
    expect(result.code).toBe(200);
    expect(result.body.toString()).toBe("hello");
    expect(result.headers).toEqual({ "content-type": "text/plain", "x-fpm-warmup": "fresh" });
    expect(result.warmup).toBe("fresh");
  });

  test("leaves optional fields unset", async () => {
    const { manager, calls } = await startServer((_call, callback) => {
      callback(null, success(204, ""));
    });

    const result = await manager.proxyRequest("http://example.com");

    expect(calls[0]?.priority).toBeUndefined();
    expect(calls[0]?.warmupUrl).toBeUndefined();
    expect(calls[0]?.retryOnCodes).toEqual([]);
    expect(result.warmup).toBeUndefined();
    expect(result.body.length).toBe(0);
  });

  test.each<WarmupStatus>(["fresh", "reused"])("surfaces x-fpm-warmup %s", async (warmup) => {
    const { manager } = await startServer((_call, callback) => {
      callback(null, success(200, "", { "x-fpm-warmup": warmup }));
    });

    const result = await manager.proxyRequest("https://example.com", {
      warmupUrl: "https://example.com/",
    });

    expect(result.warmup).toBe(warmup);
  });

  test("failed warm-up returns the warm-up response; proxyFetch rejects it", async () => {
    const { manager } = await startServer((_call, callback) => {
      callback(null, success(403, "blocked", { "X-Fpm-Warmup": "failed" }));
    });

    const result = await manager.proxyRequest("https://example.com/a", {
      warmupUrl: "https://example.com/",
    });

    expect(result).toMatchObject({ code: 403, warmup: "failed" });
    expect(result.body.toString()).toBe("blocked");
    expect(
      await rejection(
        manager.proxyFetch("https://example.com/a", { warmupUrl: "https://example.com/" }),
      ),
    ).toMatchObject({
      message: "Failed to download https://example.com/a, status code 403",
    });
  });

  test("proxyFetch decodes text and json with a schema", async () => {
    const { manager } = await startServer((_call, callback) => {
      callback(null, success(200, JSON.stringify({ name: "fpm" })));
    });

    const response = await manager.proxyFetch("https://example.com");

    expect(await response.text()).toBe('{"name":"fpm"}');
    expect(await response.json(z.object({ name: z.string() }))).toEqual({ name: "fpm" });
    expect(await rejection(response.json(z.object({ name: z.number() })))).toBeInstanceOf(ZodError);
  });
});

describe("validation", () => {
  test.each([
    ["relative url", "/path", {}],
    ["ftp url", "ftp://example.com", {}],
    ["relative warmup url", "https://example.com", { warmupUrl: "/warm" }],
    ["non-http warmup url", "https://example.com", { warmupUrl: "ftp://example.com" }],
    ["fractional priority", "https://example.com", { priority: 1.5 }],
    ["retry code out of range", "https://example.com", { retryOnCodes: [700] }],
  ])("rejects %s without calling the server", async (_name, url, options) => {
    const { manager, calls } = await startServer((_call, callback) => {
      callback(null, success(200, ""));
    });

    expect(await rejection(manager.proxyRequest(url, options))).toBeInstanceOf(ZodError);
    expect(calls).toHaveLength(0);
  });

  test.each([{ semaphore: 0 }, { maxRetries: -1 }, { clientTimeout: 1.5 }])(
    "rejects options %o",
    (options) => {
      expect(() => new ForwardProxyManager("127.0.0.1:1", options)).toThrow(ZodError);
    },
  );
});

describe("errors", () => {
  test.each([
    [ProxyErrorType.INVALID_URL, "request failed: INVALID_URL"],
    [ProxyErrorType.REMOTE_HOST_UNREACHABLE, "request failed: REMOTE_HOST_UNREACHABLE"],
  ])("maps ProxyResponseError %d", async (errorType, message) => {
    const { manager } = await startServer((_call, callback) => {
      callback(null, {
        response: { $case: "error", value: { errorType, body: Buffer.from("detail") } },
      });
    });

    const error = await rejection(manager.proxyRequest("https://example.com"));

    expect(error).toBeInstanceOf(ForwardProxyError);
    expect(error).toMatchObject({ errorType, message });
    expect(error instanceof ForwardProxyError && error.body?.toString()).toBe("detail");
  });

  test("rejects a connection failure", async () => {
    const manager = new ForwardProxyManager("127.0.0.1:1", { logger: null, clientTimeout: 200 });

    managers.push(manager);

    expect(await rejection(manager.proxyRequest("https://example.com"))).toBeInstanceOf(
      ForwardProxyConnectionError,
    );
  });
});

describe("retries", () => {
  test("retries UNAVAILABLE up to maxRetries", async () => {
    const { manager, calls } = await startServer(
      (_call, callback) => {
        if (calls.length === 1) {
          callback(serviceError(status.UNAVAILABLE, "restarting"));
        } else {
          callback(null, success(200, "ok"));
        }
      },
      { maxRetries: 1 },
    );

    const retried: string[] = [];

    manager.on("retry", (url) => retried.push(url));

    const result = await manager.proxyRequest("https://example.com");

    expect(result.body.toString()).toBe("ok");
    expect(calls).toHaveLength(2);
    expect(retried).toEqual(["https://example.com"]);
  });

  test("gives up after maxRetries", async () => {
    const { manager, calls } = await startServer((_call, callback) => {
      callback(serviceError(status.UNAVAILABLE, "down"));
    });

    expect(await rejection(manager.proxyRequest("https://example.com"))).toMatchObject({
      code: status.UNAVAILABLE,
    });
    expect(calls).toHaveLength(1);
  });

  test("does not retry other gRPC errors", async () => {
    const { manager, calls } = await startServer(
      (_call, callback) => {
        callback(serviceError(status.INVALID_ARGUMENT, "bad retry code"));
      },
      { maxRetries: 3 },
    );

    expect(await rejection(manager.proxyRequest("https://example.com"))).toMatchObject({
      code: status.INVALID_ARGUMENT,
    });
    expect(calls).toHaveLength(1);
  });
});

describe("concurrency", () => {
  test("semaphore limits requests in flight", async () => {
    const pending: sendUnaryData<ProxyResponse>[] = [];
    let maxActive = 0;

    const { manager, waitForCalls } = await startServer(
      (_call, callback) => {
        pending.push(callback);
        maxActive = Math.max(maxActive, pending.length);
      },
      { semaphore: 2 },
    );

    const total = 6;

    const requests = Array.from({ length: total }, (_, index) =>
      manager.proxyRequest(`https://example.com/${index}`),
    );

    // Answer one call at a time; a broken semaphore lets queued calls pile up on the server.
    for (let answered = 0; answered < total; answered++) {
      // oxlint-disable-next-line no-await-in-loop
      await waitForCalls(Math.min(answered + 2, total));
      pending.shift()?.(null, success(200, "ok"));
    }

    await Promise.all(requests);
    expect(maxActive).toBe(2);
  });
});

describe("cancellation", () => {
  test("aborting an in-flight request cancels the call", async () => {
    const { promise: cancelled, resolve: onCancelled } = Promise.withResolvers<void>();

    const { manager, waitForCalls } = await startServer((call) => {
      call.on("cancelled", onCancelled);
    });

    const controller = new AbortController();
    const request = manager.proxyRequest("https://example.com", { signal: controller.signal });

    await waitForCalls(1);
    controller.abort();

    expect(await rejection(request)).toMatchObject({ name: "AbortError" });
    await cancelled;
  });

  test("aborting a queued request releases its permit", async () => {
    const pending: sendUnaryData<ProxyResponse>[] = [];

    const { manager, calls, waitForCalls } = await startServer(
      (_call, callback) => {
        pending.push(callback);
      },
      { semaphore: 1 },
    );

    const first = manager.proxyRequest("https://example.com/first");

    await waitForCalls(1);

    const controller = new AbortController();

    const queued = manager.proxyRequest("https://example.com/queued", {
      signal: controller.signal,
    });

    controller.abort();
    expect(await rejection(queued)).toMatchObject({ name: "AbortError" });

    const third = manager.proxyRequest("https://example.com/third");

    pending.shift()?.(null, success(200, "first"));
    expect((await first).body.toString()).toBe("first");

    await waitForCalls(2);
    pending.shift()?.(null, success(200, "third"));
    expect((await third).body.toString()).toBe("third");
    expect(calls.map((call) => call.url)).toEqual([
      "https://example.com/first",
      "https://example.com/third",
    ]);
  });

  test("an already aborted signal never reaches the server", async () => {
    const { manager, calls } = await startServer((_call, callback) => {
      callback(null, success(200, ""));
    });

    expect(
      await rejection(manager.proxyRequest("https://example.com", { signal: AbortSignal.abort() })),
    ).toMatchObject({
      name: "AbortError",
    });
    expect(calls).toHaveLength(0);
  });
});
