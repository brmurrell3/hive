// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

import * as http from "node:http";

/**
 * Handler function for a capability invocation.
 * Receives the inputs map and returns an outputs map.
 */
export type CapabilityHandler = (
  inputs: Record<string, unknown>
) => Promise<Record<string, unknown>>;

/**
 * Options for constructing a HiveAgent.
 * All fields are optional and fall back to environment variables.
 */
export interface HiveAgentOptions {
  /** Port for the callback HTTP server. Default: HIVE_CALLBACK_PORT or 9200. */
  callbackPort?: number;
  /** Agent identifier. Default: HIVE_AGENT_ID. */
  agentId?: string;
  /** Team identifier. Default: HIVE_TEAM_ID. */
  teamId?: string;
  /** Sidecar API URL. Default: HIVE_SIDECAR_URL or http://127.0.0.1:9100. */
  sidecarUrl?: string;
  /** Workspace directory. Default: HIVE_WORKSPACE. */
  workspace?: string;
}

/**
 * Error response structure returned on capability failures.
 */
interface ErrorResponse {
  error: {
    code: string;
    message: string;
  };
}

/**
 * Success response structure returned from capability handlers.
 */
interface SuccessResponse {
  outputs: Record<string, unknown>;
}

/**
 * Health response structure returned from GET /health.
 */
interface HealthResponse {
  status: string;
}

/**
 * HiveAgent implements an HTTP callback server that the Hive sidecar calls
 * to invoke capabilities on this agent. It also provides a method to invoke
 * capabilities on remote agents via the sidecar's invoke-remote endpoint.
 *
 * Usage:
 * ```typescript
 * const agent = new HiveAgent();
 * agent.capability('greet', async (inputs) => {
 *   return { message: `Hello, ${inputs.name}!` };
 * });
 * agent.run();
 * ```
 */
export class HiveAgent {
  private readonly _agentId: string;
  private readonly _teamId: string;
  private readonly _sidecarUrl: string;
  private readonly _workspace: string;
  private readonly _callbackPort: number;
  private readonly _handlers: Map<string, CapabilityHandler> = new Map();
  private _server: http.Server | null = null;

  constructor(options?: HiveAgentOptions) {
    const opts = options ?? {};
    this._callbackPort =
      opts.callbackPort ??
      parseInt(process.env.HIVE_CALLBACK_PORT ?? "9200", 10);
    this._agentId = opts.agentId ?? process.env.HIVE_AGENT_ID ?? "";
    this._teamId = opts.teamId ?? process.env.HIVE_TEAM_ID ?? "";
    this._sidecarUrl =
      opts.sidecarUrl ??
      process.env.HIVE_SIDECAR_URL ??
      "http://127.0.0.1:9100";
    this._workspace = opts.workspace ?? process.env.HIVE_WORKSPACE ?? "";
  }

  /** The agent identifier, read from options or HIVE_AGENT_ID. */
  get agentId(): string {
    return this._agentId;
  }

  /** The team identifier, read from options or HIVE_TEAM_ID. */
  get teamId(): string {
    return this._teamId;
  }

  /** The sidecar URL. */
  get sidecarUrl(): string {
    return this._sidecarUrl;
  }

  /** The workspace directory path. */
  get workspace(): string {
    return this._workspace;
  }

  /** The port the HTTP server listens on. */
  get callbackPort(): number {
    return this._callbackPort;
  }

  /**
   * Register a capability handler. When the sidecar POSTs to
   * /handle/{name}, the handler is called with the inputs from the request
   * body and should return the outputs map.
   *
   * @param name - The capability name (must match the route the sidecar calls).
   * @param handler - Async function that processes inputs and returns outputs.
   * @throws Error if a handler is already registered for the given name.
   */
  capability(name: string, handler: CapabilityHandler): void {
    if (!name) {
      throw new Error("capability name must not be empty");
    }
    if (this._handlers.has(name)) {
      throw new Error(`capability "${name}" is already registered`);
    }
    this._handlers.set(name, handler);
  }

  /**
   * Invoke a capability on a remote agent via the sidecar's invoke-remote
   * endpoint. Sends a POST to {sidecarUrl}/capabilities/{capability}/invoke-remote
   * with the target agent, inputs, and optional timeout.
   *
   * @param target - The target agent ID.
   * @param capability - The capability name to invoke.
   * @param inputs - Key-value inputs for the capability.
   * @param timeout - Timeout duration string (e.g. "30s"). Defaults to "30s".
   * @returns The outputs map from the remote agent's response.
   * @throws Error if the invocation fails or the remote agent returns an error.
   */
  async invoke(
    target: string,
    capability: string,
    inputs: Record<string, unknown>,
    timeout?: string
  ): Promise<Record<string, unknown>> {
    const url = `${this._sidecarUrl}/capabilities/${encodeURIComponent(capability)}/invoke-remote`;
    const body = JSON.stringify({
      target,
      inputs,
      timeout: timeout ?? "30s",
    });

    const responseBody = await this._httpPost(url, body);
    const parsed = JSON.parse(responseBody);

    if (parsed.status === "error" || parsed.error) {
      const errMsg =
        parsed.error?.message ?? parsed.error ?? "remote invocation failed";
      throw new Error(errMsg);
    }

    return (parsed.outputs as Record<string, unknown>) ?? {};
  }

  /**
   * Start the HTTP callback server. The server listens on 127.0.0.1 at the
   * configured callback port. This method blocks (the server runs until the
   * process is terminated).
   *
   * Handles SIGTERM and SIGINT for graceful shutdown.
   */
  run(): void {
    this._server = http.createServer(
      (req: http.IncomingMessage, res: http.ServerResponse) => {
        this._handleRequest(req, res);
      }
    );

    this._server.listen(this._callbackPort, "127.0.0.1", () => {
      const logMsg = `[hive-sdk] Agent listening on 127.0.0.1:${this._callbackPort}`;
      if (this._agentId) {
        process.stderr.write(`${logMsg} (agent=${this._agentId})\n`);
      } else {
        process.stderr.write(`${logMsg}\n`);
      }
    });

    // Graceful shutdown on signals.
    const shutdown = (): void => {
      if (this._server) {
        this._server.close();
        this._server = null;
      }
    };

    process.on("SIGTERM", shutdown);
    process.on("SIGINT", shutdown);
  }

  /**
   * Stop the HTTP server. Useful for testing.
   */
  stop(): void {
    if (this._server) {
      this._server.close();
      this._server = null;
    }
  }

  /**
   * Route incoming HTTP requests to the appropriate handler.
   */
  private _handleRequest(
    req: http.IncomingMessage,
    res: http.ServerResponse
  ): void {
    const url = req.url ?? "/";
    const method = req.method ?? "GET";

    // GET /health
    if (method === "GET" && url === "/health") {
      this._sendJSON(res, 200, { status: "healthy" } satisfies HealthResponse);
      return;
    }

    // POST /handle/{capability_name}
    if (method === "POST" && url.startsWith("/handle/")) {
      const capName = url.slice("/handle/".length);
      if (!capName) {
        this._sendJSON(res, 404, {
          error: { code: "NOT_FOUND", message: "missing capability name" },
        } satisfies ErrorResponse);
        return;
      }

      this._handleCapability(capName, req, res);
      return;
    }

    // Everything else: 404
    this._sendJSON(res, 404, {
      error: { code: "NOT_FOUND", message: `no route for ${method} ${url}` },
    } satisfies ErrorResponse);
  }

  /**
   * Handle a capability invocation request: read the body, look up the
   * handler, execute it, and return the result.
   */
  private _handleCapability(
    capName: string,
    req: http.IncomingMessage,
    res: http.ServerResponse
  ): void {
    const handler = this._handlers.get(capName);
    if (!handler) {
      this._sendJSON(res, 404, {
        error: {
          code: "CAPABILITY_NOT_FOUND",
          message: `no handler registered for "${capName}"`,
        },
      } satisfies ErrorResponse);
      return;
    }

    this._readBody(req)
      .then((rawBody) => {
        let inputs: Record<string, unknown> = {};
        if (rawBody.length > 0) {
          try {
            const parsed = JSON.parse(rawBody);
            inputs = (parsed.inputs as Record<string, unknown>) ?? {};
          } catch {
            this._sendJSON(res, 400, {
              error: {
                code: "INVALID_JSON",
                message: "request body is not valid JSON",
              },
            } satisfies ErrorResponse);
            return;
          }
        }

        handler(inputs)
          .then((outputs) => {
            this._sendJSON(res, 200, {
              outputs,
            } satisfies SuccessResponse);
          })
          .catch((err: unknown) => {
            const message =
              err instanceof Error ? err.message : String(err);
            this._sendJSON(res, 500, {
              error: { code: "CAPABILITY_FAILED", message },
            } satisfies ErrorResponse);
          });
      })
      .catch(() => {
        this._sendJSON(res, 400, {
          error: {
            code: "READ_ERROR",
            message: "failed to read request body",
          },
        } satisfies ErrorResponse);
      });
  }

  /**
   * Read the full body of an incoming HTTP request.
   */
  private _readBody(req: http.IncomingMessage): Promise<string> {
    return new Promise((resolve, reject) => {
      const chunks: Buffer[] = [];
      let totalSize = 0;
      const maxSize = 1 << 20; // 1MB limit

      req.on("data", (chunk: Buffer) => {
        totalSize += chunk.length;
        if (totalSize > maxSize) {
          req.destroy();
          reject(new Error("request body too large"));
          return;
        }
        chunks.push(chunk);
      });

      req.on("end", () => {
        resolve(Buffer.concat(chunks).toString("utf-8"));
      });

      req.on("error", (err: Error) => {
        reject(err);
      });
    });
  }

  /**
   * Send a JSON response.
   */
  private _sendJSON(
    res: http.ServerResponse,
    statusCode: number,
    body: HealthResponse | SuccessResponse | ErrorResponse | Record<string, unknown>
  ): void {
    const json = JSON.stringify(body);
    res.writeHead(statusCode, {
      "Content-Type": "application/json",
      "Content-Length": Buffer.byteLength(json),
    });
    res.end(json);
  }

  /**
   * Perform an HTTP POST request using the built-in http/https modules.
   * Returns the response body as a string.
   */
  private _httpPost(url: string, body: string): Promise<string> {
    return new Promise((resolve, reject) => {
      const parsedUrl = new URL(url);
      const isHttps = parsedUrl.protocol === "https:";

      // Dynamically require https only when needed (still zero-dep, just
      // using the Node.js built-in https module).
      const transport = isHttps
        ? // eslint-disable-next-line @typescript-eslint/no-require-imports
          require("node:https")
        : http;

      const options: http.RequestOptions = {
        hostname: parsedUrl.hostname,
        port: parsedUrl.port || (isHttps ? 443 : 80),
        path: parsedUrl.pathname + parsedUrl.search,
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Content-Length": Buffer.byteLength(body),
        },
      };

      const req = transport.request(
        options,
        (res: http.IncomingMessage) => {
          const chunks: Buffer[] = [];

          res.on("data", (chunk: Buffer) => {
            chunks.push(chunk);
          });

          res.on("end", () => {
            const responseBody = Buffer.concat(chunks).toString("utf-8");
            const statusCode = res.statusCode ?? 0;
            if (statusCode >= 200 && statusCode < 300) {
              resolve(responseBody);
            } else {
              reject(
                new Error(
                  `HTTP ${statusCode}: ${responseBody}`
                )
              );
            }
          });

          res.on("error", reject);
        }
      );

      req.on("error", reject);
      req.write(body);
      req.end();
    });
  }
}
