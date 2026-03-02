/**
 * Handler function for a capability invocation.
 * Receives the inputs map and returns an outputs map.
 */
export type CapabilityHandler = (inputs: Record<string, unknown>) => Promise<Record<string, unknown>>;
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
export declare class HiveAgent {
    private readonly _agentId;
    private readonly _teamId;
    private readonly _sidecarUrl;
    private readonly _workspace;
    private readonly _callbackPort;
    private readonly _handlers;
    private _server;
    constructor(options?: HiveAgentOptions);
    /** The agent identifier, read from options or HIVE_AGENT_ID. */
    get agentId(): string;
    /** The team identifier, read from options or HIVE_TEAM_ID. */
    get teamId(): string;
    /** The sidecar URL. */
    get sidecarUrl(): string;
    /** The workspace directory path. */
    get workspace(): string;
    /** The port the HTTP server listens on. */
    get callbackPort(): number;
    /**
     * Register a capability handler. When the sidecar POSTs to
     * /handle/{name}, the handler is called with the inputs from the request
     * body and should return the outputs map.
     *
     * @param name - The capability name (must match the route the sidecar calls).
     * @param handler - Async function that processes inputs and returns outputs.
     * @throws Error if a handler is already registered for the given name.
     */
    capability(name: string, handler: CapabilityHandler): void;
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
    invoke(target: string, capability: string, inputs: Record<string, unknown>, timeout?: string): Promise<Record<string, unknown>>;
    /**
     * Start the HTTP callback server. The server listens on 127.0.0.1 at the
     * configured callback port. This method blocks (the server runs until the
     * process is terminated).
     *
     * Handles SIGTERM and SIGINT for graceful shutdown.
     */
    run(): void;
    /**
     * Stop the HTTP server. Useful for testing.
     */
    stop(): void;
    /**
     * Route incoming HTTP requests to the appropriate handler.
     */
    private _handleRequest;
    /**
     * Handle a capability invocation request: read the body, look up the
     * handler, execute it, and return the result.
     */
    private _handleCapability;
    /**
     * Read the full body of an incoming HTTP request.
     */
    private _readBody;
    /**
     * Send a JSON response.
     */
    private _sendJSON;
    /**
     * Perform an HTTP POST request using the built-in http/https modules.
     * Returns the response body as a string.
     */
    private _httpPost;
}
//# sourceMappingURL=index.d.ts.map