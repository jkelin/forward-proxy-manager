import { ChannelCredentials, connectivityState, status } from "@grpc/grpc-js";
import { Semaphore, delay, mapKeys } from "es-toolkit";
import { EventEmitter } from "node:events";
import { promisify, type DebugLoggerFunction } from "node:util";
import { z } from "zod";

import {
 ProxyClient,
 ProxyResponseError_ErrorType,
 proxyResponseError_ErrorTypeToJSON,
 type ProxyRequest,
 type ProxyResponse,
} from "./generated/service.js";

export { ProxyResponseError_ErrorType as ProxyErrorType } from "./generated/service.js";

export interface ForwardProxyLogger {
 debug: DebugLoggerFunction;
 error: DebugLoggerFunction;
 info: DebugLoggerFunction;
}

export interface ForwardProxyManagerOptions {
 /** Max requests in flight from this client. */
 semaphore?: number;
 /** Retries of UNAVAILABLE / connection failures. */
 maxRetries?: number;
 /** Milliseconds to wait for the gRPC channel to become ready. */
 clientTimeout?: number;
 /** `null` disables logging. */
 logger?: ForwardProxyLogger | null;
}

export interface ProxyRequestOptions {
 signal?: AbortSignal;
 /** Higher priority is processed first. */
 priority?: number;
 /** Target HTTP status codes that make the server retry on another proxy. */
 retryOnCodes?: number[];
 /** Absolute http(s) URL fetched first when the chosen proxy has no session for the target host. */
 warmupUrl?: string;
}

/** Value of the `x-fpm-warmup` response header. */
export type WarmupStatus = "fresh" | "reused" | "failed";

export interface ProxyRequestResult {
 body: Buffer;
 code: number;
 /** Response headers with lower-cased names. */
 headers: Record<string, string>;
 /** Present only when `warmupUrl` was sent. On `failed` the result is the warm-up response. */
 warmup: WarmupStatus | undefined;
}

export interface ProxyFetchResult {
 headers: Record<string, string>;
 warmup: WarmupStatus | undefined;
 text: () => Promise<string>;
 json: <T>(schema: z.ZodType<T>) => Promise<T>;
 buffer: () => Promise<Buffer>;
}

/** The server answered with a ProxyResponseError. */
export class ForwardProxyError extends Error {
 readonly errorType: ProxyResponseError_ErrorType;
 readonly body: Buffer | undefined;

 constructor(errorType: ProxyResponseError_ErrorType, body: Buffer | undefined) {
  super(`request failed: ${proxyResponseError_ErrorTypeToJSON(errorType)}`);
  this.name = "ForwardProxyError";
  this.errorType = errorType;
  this.body = body;
 }
}

/** The gRPC channel did not become ready within `clientTimeout`. */
export class ForwardProxyConnectionError extends Error {
 constructor(url: string, cause: Error) {
  super(`Failed to connect to proxy at ${url}`, { cause });
  this.name = "ForwardProxyConnectionError";
 }
}

const httpUrlSchema = z.url({ protocol: /^https?$/ });

const managerOptionsSchema = z.object({
 semaphore: z.int().positive().default(1000),
 maxRetries: z.int().nonnegative().default(0),
 clientTimeout: z.int().positive().default(5000),
});

const requestOptionsSchema = z.object({
 priority: z.int().optional(),
 retryOnCodes: z.array(z.int().min(100).max(599)).optional(),
 warmupUrl: httpUrlSchema.optional(),
});

const warmupSchema = z.enum(["fresh", "reused", "failed"]).optional();

const consoleLogger: ForwardProxyLogger = {
 debug: console.debug,
 error: console.error,
 info: console.info,
};

const maxBackoffMs = 10_000;

// Connection failures and UNAVAILABLE (server restarting, connection dropped) are transient;
// every other gRPC status and every ProxyResponseError is final.
function isRetryable(error: Error): boolean {
 if (error instanceof ForwardProxyConnectionError) {
  return true;
 }

 return "code" in error && error.code === status.UNAVAILABLE;
}

export class ForwardProxyManager extends EventEmitter<{
 connecting: [];
 connected: [];
 download: [string];
 retry: [string];
}> {
 private readonly url: string;
 private readonly semaphore: Semaphore;
 private readonly maxRetries: number;
 private readonly clientTimeout: number;
 private readonly logger: ForwardProxyLogger | undefined;
 private clientPromise: Promise<ProxyClient> | undefined;

 /** @param url gRPC target, e.g. `localhost:8082`. */
 constructor(url: string, options: ForwardProxyManagerOptions = {}) {
  super();

  const parsed = managerOptionsSchema.parse(options);

  this.url = z.string().min(1).parse(url);
  this.semaphore = new Semaphore(parsed.semaphore);
  this.maxRetries = parsed.maxRetries;
  this.clientTimeout = parsed.clientTimeout;
  this.logger = options.logger === undefined ? consoleLogger : (options.logger ?? undefined);
 }

 private async connectProxyClient(): Promise<ProxyClient> {
  this.emit("connecting");
  this.logger?.info(`[ForwardProxyManager] Connecting to proxy at ${this.url}`);

  const client = new ProxyClient(this.url, ChannelCredentials.createInsecure(), {
   "grpc.max_receive_message_length": Number.MAX_SAFE_INTEGER,
   "grpc.max_send_message_length": Number.MAX_SAFE_INTEGER,
   "grpc.max_reconnect_backoff_ms": 1000,
   "grpc.default_compression_level": 2,
   "grpc.enable_retries": 1,
  });

  const channel = client.getChannel();

  // Keeps the channel connecting while idle; stops once `close()` shuts it down,
  // because watching a closed channel throws.
  const kickChannel = (): void => {
   const state = channel.getConnectivityState(true);

   if (state === connectivityState.SHUTDOWN) {
    return;
   }

   this.logger?.debug(`[ForwardProxyManager] Current state ${connectivityState[state]}`);
   channel.watchConnectivityState(state, Infinity, kickChannel);
  };

  kickChannel();

  try {
   await promisify(client.waitForReady.bind(client))(new Date(Date.now() + this.clientTimeout));
  } catch (error) {
   client.close();

   throw new ForwardProxyConnectionError(this.url, error instanceof Error ? error : new Error(String(error)));
  }

  this.emit("connected");
  this.logger?.info("[ForwardProxyManager] Connected to proxy server");

  return client;
 }

 /** Connects once and reuses the client; a failed connection is retried on the next call. */
 public async connect(): Promise<ProxyClient> {
  this.clientPromise ??= this.connectProxyClient().catch((error: Error) => {
   this.clientPromise = undefined;

   throw error;
  });

  return await this.clientPromise;
 }

 /** Closes the gRPC channel. Later requests reconnect. */
 public close(): void {
  const clientPromise = this.clientPromise;

  this.clientPromise = undefined;
  void clientPromise?.then((client) => client.close()).catch(() => undefined);
 }

 // Resolves once a semaphore permit is held. When the signal aborts while queued, the
 // permit acquired later is released immediately so queued cancellations never leak permits.
 private async acquire(signal: AbortSignal | undefined): Promise<void> {
  signal?.throwIfAborted();

  const acquired = this.semaphore.acquire();

  if (!signal) {
   return await acquired;
  }

  await new Promise<void>((resolve, reject) => {
   const onAbort = (): void => {
    void acquired.then(() => this.semaphore.release());
    reject(signal.reason);
   };

   const onAcquired = (): void => {
    signal.removeEventListener("abort", onAbort);
    resolve();
   };

   signal.addEventListener("abort", onAbort, { once: true });
   void acquired.then(onAcquired);
  });
 }

 private async sendWithPermit(request: ProxyRequest, signal: AbortSignal | undefined): Promise<ProxyRequestResult> {
  await this.acquire(signal);

  try {
   this.emit("download", request.url);
   this.logger?.debug(`[ForwardProxyManager] Downloading ${request.url}`);

   return await this.send(request, signal);
  } finally {
   this.semaphore.release();
  }
 }

 private async send(request: ProxyRequest, signal: AbortSignal | undefined): Promise<ProxyRequestResult> {
  const client = await this.connect();

  signal?.throwIfAborted();

  const response = await new Promise<ProxyResponse>((resolve, reject) => {
   const onAbort = (): void => {
    call.cancel();
   };

   const call = client.sendRequest(request, (error, value) => {
    signal?.removeEventListener("abort", onAbort);

    if (signal?.aborted) {
     reject(signal.reason);
    } else if (error) {
     reject(error);
    } else {
     resolve(value);
    }
   });

   signal?.addEventListener("abort", onAbort, { once: true });
  });

  const result = response.response;

  if (result === undefined) {
   throw new Error("No response");
  }

  if (result.$case === "error") {
   throw new ForwardProxyError(result.value.errorType, result.value.body);
  }

  const headers = mapKeys(result.value.headers, (_value, key) => key.toLowerCase());

  return {
   body: result.value.body ?? Buffer.alloc(0),
   code: result.value.status,
   headers,
   warmup: warmupSchema.parse(headers["x-fpm-warmup"]),
  };
 }

 /**
  * Sends one request through the proxy. Rejects with a ZodError on invalid input,
  * ForwardProxyError on a server error response, and an AbortError when `signal` aborts.
  */
 async proxyRequest(url: string, options: ProxyRequestOptions = {}): Promise<ProxyRequestResult> {
  const parsed = requestOptionsSchema.parse(options);
  const signal = options.signal;

  const request: ProxyRequest = {
   url: httpUrlSchema.parse(url),
   method: "",
   headers: {},
   priority: parsed.priority,
   retryOnCodes: parsed.retryOnCodes ?? [],
   warmupUrl: parsed.warmupUrl,
  };

  // Attempts are sequential by design: each retry waits for the previous failure and backoff.
  for (let attempt = 0; ; attempt++) {
   try {
    // oxlint-disable-next-line no-await-in-loop
    return await this.sendWithPermit(request, signal);
   } catch (error) {
    const failure = error instanceof Error ? error : new Error(String(error));

    if (signal?.aborted || !isRetryable(failure) || attempt >= this.maxRetries) {
     throw failure;
    }

    this.emit("retry", url);
    this.logger?.error(`[ForwardProxyManager] Failed to download ${url}, retrying...`);

    // The permit is released before backing off so waiting retries do not block other requests.
    // oxlint-disable-next-line no-await-in-loop
    await delay(Math.min((attempt + 1) * 250, maxBackoffMs), { signal });
   }
  }
 }

 /** Like `proxyRequest`, but rejects when the target status code is not 200. */
 async proxyFetch(url: string, options: ProxyRequestOptions = {}): Promise<ProxyFetchResult> {
  const { body, code, headers, warmup } = await this.proxyRequest(url, options);

  if (code !== 200) {
   throw new Error(`Failed to download ${url}, status code ${code}`);
  }

  return {
   headers,
   warmup,
   text: async () => body.toString("utf-8"),
   json: async <T>(schema: z.ZodType<T>) => schema.parse(JSON.parse(body.toString("utf-8"))),
   buffer: async () => body,
  };
 }
}
