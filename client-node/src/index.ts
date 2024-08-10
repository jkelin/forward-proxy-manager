import { Sema } from "async-sema";
import grpc, { ChannelCredentials, connectivityState } from "@grpc/grpc-js";
import { proxy } from "./service.js";
import { DebugLoggerFunction, promisify } from "util";
import EventEmitter from "node:events";

interface ILogger {
  debug: DebugLoggerFunction;
  error: DebugLoggerFunction;
  info: DebugLoggerFunction;
}

export class ForwardProxyManager extends EventEmitter<{
  connecting: [];
  connected: [];
  download: [string];
  retry: [string];
}> {
  private semaphore: Sema;
  private maxRetries: number;
  private clientTimeout: number;
  private clientPromise?: Promise<proxy.ProxyClient>;
  private logger?: ILogger;

  constructor(
    private url: string,
    {
      semaphore = 1000,
      maxRetries = 0,
      clientTimeout = 1000 * 5,
      logger = {
        debug: console.debug,
        error: console.error,
        info: console.info,
      },
    }: {
      semaphore?: number;
      maxRetries?: number;
      clientTimeout?: number;
      logger?: ILogger | null;
    } = {}
  ) {
    super();

    this.semaphore = new Sema(semaphore);
    this.maxRetries = maxRetries;
    this.clientTimeout = clientTimeout;
    this.logger = logger ?? undefined;
  }

  private async connectProxyClient() {
    this.emit("connecting");
    this.logger?.info(
      `[ForwardProxyManager] Connecting to proxy at ${this.url}`
    );

    const client = new proxy.ProxyClient(
      this.url,
      ChannelCredentials.createInsecure(),
      {
        "grpc.max_receive_message_length": Number.MAX_SAFE_INTEGER,
        "grpc.max_send_message_length": Number.MAX_SAFE_INTEGER,
        "grpc.max_reconnect_backoff_ms": 1000,
        "grpc.default_compression_level": 2,
        "grpc.enable_retries": 1,
      }
    );

    const channel = client.getChannel();

    const kickChannel = () => {
      const state = channel.getConnectivityState(true);
      this.logger?.debug(
        `[ForwardProxyManager] Current state ${connectivityState[state]}`
      );
      channel.watchConnectivityState(state, Infinity, kickChannel);
    };

    kickChannel();

    await promisify(client.waitForReady.bind(client))(
      new Date(Date.now() + this.clientTimeout)
    );

    this.emit("connected");
    this.logger?.info("[ForwardProxyManager] Connected to proxy server");

    return client;
  }

  public async connect() {
    if (!this.clientPromise) {
      this.clientPromise = this.connectProxyClient();
    }

    return await this.clientPromise;
  }

  private async proxyRequestInternal(
    url: string,
    {
      signal,
      priority,
      retryOnCodes,
    }: {
      signal?: AbortSignal;
      priority?: number;
      retryOnCodes?: number[];
    } = {}
  ): Promise<{ body: Buffer; code: number }> {
    const client = await this.connect();

    return await new Promise<{ body: Buffer; code: number }>(
      (resolve, reject) => {
        const request = client.SendRequest(
          new proxy.ProxyRequest({
            url,
            priority: priority,
            retry_on_codes: retryOnCodes,
          }),
          {},
          (err, value) => {
            if (err) {
              if (/connect/i.test(err.message)) {
                (err as any).retryable = true;
                reject(err);
              }

              reject(err);
            } else if (!value) {
              reject(new Error("No response"));
            } else if (value.has_error) {
              const error =
                proxy.ProxyResponseError.ErrorType[value.error.error_type];

              reject(new Error(`request failed: ${error}`));
            } else if (!value.has_success) {
              reject(new Error("No response"));
            } else {
              resolve({
                body: Buffer.from(value.success.body),
                code: value.success.status,
              });
            }
          }
        );

        if (signal) {
          const listener = () => {
            request.cancel();
            reject(new Error("Request aborted"));
          };

          signal.addEventListener("abort", listener);

          request.on("status", () => {
            signal.removeEventListener("abort", listener);
          });
        }
      }
    );
  }

  async proxyRequest(
    url: string,
    opts: {
      signal?: AbortSignal;
      priority?: number;
      retryOnCodes?: number[];
    } = {}
  ): Promise<{ body: Buffer; code: number }> {
    let retries = 0;

    while (true) {
      await this.semaphore.acquire();
      try {
        this.emit("download", url);
        this.logger?.debug(`[ForwardProxyManager] Downloading ${url}`);

        return await this.proxyRequestInternal(url, opts);
      } catch (err) {
        if ((err as any).retryable) {
          this.emit("retry", url);
          this.logger?.error(
            `[ForwardProxyManager] Failed to download ${url}, retrying...`
          );

          retries++;

          await new Promise((resolve) =>
            setTimeout(resolve, Math.min(retries * 250, 1000 * 10))
          );

          if (retries > this.maxRetries) {
            throw err;
          }

          continue;
        }
      } finally {
        this.semaphore.release();
      }
    }
  }

  async proxyFetch(
    url: string,
    opts: {
      signal?: AbortSignal;
      priority?: number;
      retryOnCodes?: number[];
    } = {}
  ) {
    const { body, code } = await this.proxyRequest(url, opts);

    if (code !== 200) {
      throw new Error(`Failed to download ${url}, status code ${code}`);
    }

    return {
      text: async () => body.toString("utf-8"),
      json: async () => JSON.parse(body.toString("utf-8")),
      buffer: async () => body,
    };
  }
}
