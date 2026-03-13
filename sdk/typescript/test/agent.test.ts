// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

import { describe, it, before, after, beforeEach, afterEach } from "node:test";
import * as assert from "node:assert/strict";
import * as http from "node:http";
import { HiveAgent } from "../src/index";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Make an HTTP request and return the parsed JSON response. */
async function request(
  method: string,
  url: string,
  body?: Record<string, unknown>
): Promise<{ status: number; body: Record<string, unknown> }> {
  return new Promise((resolve, reject) => {
    const parsedUrl = new URL(url);
    const payload = body ? JSON.stringify(body) : undefined;

    const options: http.RequestOptions = {
      hostname: parsedUrl.hostname,
      port: parsedUrl.port,
      path: parsedUrl.pathname,
      method,
      headers: payload
        ? {
            "Content-Type": "application/json",
            "Content-Length": Buffer.byteLength(payload),
          }
        : {},
    };

    const req = http.request(options, (res) => {
      const chunks: Buffer[] = [];
      res.on("data", (chunk: Buffer) => chunks.push(chunk));
      res.on("end", () => {
        const raw = Buffer.concat(chunks).toString("utf-8");
        let parsed: Record<string, unknown> = {};
        try {
          parsed = JSON.parse(raw) as Record<string, unknown>;
        } catch {
          parsed = { raw };
        }
        resolve({ status: res.statusCode ?? 0, body: parsed });
      });
      res.on("error", reject);
    });

    req.on("error", reject);
    if (payload) req.write(payload);
    req.end();
  });
}

/** Wait for a server to accept connections. */
async function waitForServer(
  port: number,
  maxRetries = 20
): Promise<void> {
  for (let i = 0; i < maxRetries; i++) {
    try {
      const res = await request("GET", `http://127.0.0.1:${port}/health`);
      if (res.status === 200) return;
    } catch {
      // Server not ready yet.
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`Server on port ${port} did not start in time`);
}

// ---------------------------------------------------------------------------
// Tests: Capability Registration
// ---------------------------------------------------------------------------

describe("HiveAgent capability registration", () => {
  it("should register a capability handler", () => {
    const agent = new HiveAgent({ callbackPort: 0 });
    agent.capability("test-cap", async (inputs) => ({
      result: inputs.value,
    }));
    // No error means success. We verify it works via HTTP below.
  });

  it("should throw on empty capability name", () => {
    const agent = new HiveAgent({ callbackPort: 0 });
    assert.throws(
      () => agent.capability("", async () => ({})),
      /capability name must not be empty/
    );
  });

  it("should throw on duplicate capability name", () => {
    const agent = new HiveAgent({ callbackPort: 0 });
    agent.capability("dup", async () => ({}));
    assert.throws(
      () => agent.capability("dup", async () => ({})),
      /already registered/
    );
  });
});

// ---------------------------------------------------------------------------
// Tests: Environment Variable Reading
// ---------------------------------------------------------------------------

describe("HiveAgent environment reading", () => {
  const savedEnv: Record<string, string | undefined> = {};

  before(() => {
    // Save and set env vars.
    const vars = [
      "HIVE_CALLBACK_PORT",
      "HIVE_AGENT_ID",
      "HIVE_TEAM_ID",
      "HIVE_SIDECAR_URL",
      "HIVE_WORKSPACE",
    ];
    for (const v of vars) {
      savedEnv[v] = process.env[v];
    }
    process.env.HIVE_CALLBACK_PORT = "9999";
    process.env.HIVE_AGENT_ID = "env-agent";
    process.env.HIVE_TEAM_ID = "env-team";
    process.env.HIVE_SIDECAR_URL = "http://sidecar:9100";
    process.env.HIVE_WORKSPACE = "/tmp/workspace";
  });

  after(() => {
    // Restore env vars.
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) {
        delete process.env[k];
      } else {
        process.env[k] = v;
      }
    }
  });

  it("should read agent ID from env", () => {
    const agent = new HiveAgent();
    assert.equal(agent.agentId, "env-agent");
  });

  it("should read team ID from env", () => {
    const agent = new HiveAgent();
    assert.equal(agent.teamId, "env-team");
  });

  it("should read callback port from env", () => {
    const agent = new HiveAgent();
    assert.equal(agent.callbackPort, 9999);
  });

  it("should read sidecar URL from env", () => {
    const agent = new HiveAgent();
    assert.equal(agent.sidecarUrl, "http://sidecar:9100");
  });

  it("should read workspace from env", () => {
    const agent = new HiveAgent();
    assert.equal(agent.workspace, "/tmp/workspace");
  });

  it("should prefer options over env vars", () => {
    const agent = new HiveAgent({
      agentId: "opt-agent",
      teamId: "opt-team",
      callbackPort: 8888,
      sidecarUrl: "http://opt:9100",
      workspace: "/opt/workspace",
    });
    assert.equal(agent.agentId, "opt-agent");
    assert.equal(agent.teamId, "opt-team");
    assert.equal(agent.callbackPort, 8888);
    assert.equal(agent.sidecarUrl, "http://opt:9100");
    assert.equal(agent.workspace, "/opt/workspace");
  });
});

// ---------------------------------------------------------------------------
// Tests: Default Values
// ---------------------------------------------------------------------------

describe("HiveAgent defaults", () => {
  const savedEnv: Record<string, string | undefined> = {};

  before(() => {
    // Clear all HIVE_ env vars so defaults kick in.
    const vars = [
      "HIVE_CALLBACK_PORT",
      "HIVE_AGENT_ID",
      "HIVE_TEAM_ID",
      "HIVE_SIDECAR_URL",
      "HIVE_WORKSPACE",
    ];
    for (const v of vars) {
      savedEnv[v] = process.env[v];
      delete process.env[v];
    }
  });

  after(() => {
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) {
        delete process.env[k];
      } else {
        process.env[k] = v;
      }
    }
  });

  it("should default callback port to 9200", () => {
    const agent = new HiveAgent();
    assert.equal(agent.callbackPort, 9200);
  });

  it("should default sidecar URL to http://127.0.0.1:9100", () => {
    const agent = new HiveAgent();
    assert.equal(agent.sidecarUrl, "http://127.0.0.1:9100");
  });

  it("should default agent ID to empty string", () => {
    const agent = new HiveAgent();
    assert.equal(agent.agentId, "");
  });

  it("should default team ID to empty string", () => {
    const agent = new HiveAgent();
    assert.equal(agent.teamId, "");
  });
});

// ---------------------------------------------------------------------------
// Tests: HTTP Server - Health Endpoint
// ---------------------------------------------------------------------------

describe("HiveAgent HTTP server", () => {
  let agent: HiveAgent;
  // Use port 0 via options; we'll find the actual port from the server.
  // Since run() uses the configured port, we pick a high random port.
  const testPort = 19200 + Math.floor(Math.random() * 1000);

  before(async () => {
    agent = new HiveAgent({
      callbackPort: testPort,
      agentId: "test-agent",
      teamId: "test-team",
    });

    agent.capability("greet", async (inputs) => {
      return { message: `Hello, ${inputs.name}!` };
    });

    agent.capability("fail", async () => {
      throw new Error("something went wrong");
    });

    agent.capability("echo", async (inputs) => {
      return { ...inputs };
    });

    agent.run();
    await waitForServer(testPort);
  });

  after(() => {
    agent.stop();
  });

  it("GET /health returns healthy status", async () => {
    const res = await request("GET", `http://127.0.0.1:${testPort}/health`);
    assert.equal(res.status, 200);
    assert.equal(res.body.status, "healthy");
  });

  it("POST /handle/greet returns outputs", async () => {
    const res = await request(
      "POST",
      `http://127.0.0.1:${testPort}/handle/greet`,
      { inputs: { name: "World" } }
    );
    assert.equal(res.status, 200);
    const outputs = res.body.outputs as Record<string, unknown>;
    assert.equal(outputs.message, "Hello, World!");
  });

  it("POST /handle/echo echoes inputs back", async () => {
    const res = await request(
      "POST",
      `http://127.0.0.1:${testPort}/handle/echo`,
      { inputs: { foo: "bar", count: 42 } }
    );
    assert.equal(res.status, 200);
    const outputs = res.body.outputs as Record<string, unknown>;
    assert.equal(outputs.foo, "bar");
    assert.equal(outputs.count, 42);
  });

  it("POST /handle/fail returns error with 500", async () => {
    const res = await request(
      "POST",
      `http://127.0.0.1:${testPort}/handle/fail`,
      { inputs: {} }
    );
    assert.equal(res.status, 500);
    const error = res.body.error as Record<string, unknown>;
    assert.equal(error.code, "CAPABILITY_FAILED");
    assert.equal(error.message, "something went wrong");
  });

  it("POST /handle/nonexistent returns 404", async () => {
    const res = await request(
      "POST",
      `http://127.0.0.1:${testPort}/handle/nonexistent`,
      { inputs: {} }
    );
    assert.equal(res.status, 404);
    const error = res.body.error as Record<string, unknown>;
    assert.equal(error.code, "CAPABILITY_NOT_FOUND");
  });

  it("GET /nonexistent returns 404", async () => {
    const res = await request(
      "GET",
      `http://127.0.0.1:${testPort}/nonexistent`
    );
    assert.equal(res.status, 404);
  });

  it("POST /handle/greet with empty body uses empty inputs", async () => {
    const res = await request(
      "POST",
      `http://127.0.0.1:${testPort}/handle/greet`
    );
    assert.equal(res.status, 200);
    const outputs = res.body.outputs as Record<string, unknown>;
    assert.equal(outputs.message, "Hello, undefined!");
  });

  it("POST /handle/greet with invalid JSON returns 400", async () => {
    // Send raw invalid JSON.
    const res = await new Promise<{
      status: number;
      body: Record<string, unknown>;
    }>((resolve, reject) => {
      const req = http.request(
        {
          hostname: "127.0.0.1",
          port: testPort,
          path: "/handle/greet",
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "Content-Length": 11,
          },
        },
        (httpRes) => {
          const chunks: Buffer[] = [];
          httpRes.on("data", (c: Buffer) => chunks.push(c));
          httpRes.on("end", () => {
            resolve({
              status: httpRes.statusCode ?? 0,
              body: JSON.parse(
                Buffer.concat(chunks).toString("utf-8")
              ) as Record<string, unknown>,
            });
          });
          httpRes.on("error", reject);
        }
      );
      req.on("error", reject);
      req.write("not valid!!");
      req.end();
    });

    assert.equal(res.status, 400);
    const error = res.body.error as Record<string, unknown>;
    assert.equal(error.code, "INVALID_JSON");
  });
});

// ---------------------------------------------------------------------------
// Tests: invoke() (remote capability invocation via sidecar)
// ---------------------------------------------------------------------------

describe("HiveAgent.invoke()", () => {
  let mockSidecar: http.Server;
  let sidecarPort: number;

  before(async () => {
    // Start a mock sidecar that responds to invoke-remote requests.
    mockSidecar = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        const body = Buffer.concat(chunks).toString("utf-8");
        const parsed = JSON.parse(body) as Record<string, unknown>;
        const url = req.url ?? "";

        if (url.includes("/capabilities/echo/invoke-remote")) {
          const inputs = parsed.inputs as Record<string, unknown>;
          const response = JSON.stringify({
            status: "success",
            outputs: { echoed: inputs.text },
            duration_ms: 10,
          });
          res.writeHead(200, {
            "Content-Type": "application/json",
            "Content-Length": Buffer.byteLength(response),
          });
          res.end(response);
        } else if (url.includes("/capabilities/fail-cap/invoke-remote")) {
          const response = JSON.stringify({
            status: "error",
            error: {
              code: "AGENT_OFFLINE",
              message: "target agent not available",
              retryable: true,
            },
          });
          res.writeHead(502, {
            "Content-Type": "application/json",
            "Content-Length": Buffer.byteLength(response),
          });
          res.end(response);
        } else {
          res.writeHead(404);
          res.end();
        }
      });
    });

    await new Promise<void>((resolve) => {
      mockSidecar.listen(0, "127.0.0.1", () => {
        const addr = mockSidecar.address();
        if (addr && typeof addr !== "string") {
          sidecarPort = addr.port;
        }
        resolve();
      });
    });
  });

  after(() => {
    mockSidecar.close();
  });

  it("should invoke a remote capability successfully", async () => {
    const agent = new HiveAgent({
      callbackPort: 0,
      sidecarUrl: `http://127.0.0.1:${sidecarPort}`,
    });

    const result = await agent.invoke("agent-b", "echo", { text: "hello" });
    assert.equal(result.echoed, "hello");
  });

  it("should throw on remote invocation failure", async () => {
    const agent = new HiveAgent({
      callbackPort: 0,
      sidecarUrl: `http://127.0.0.1:${sidecarPort}`,
    });

    await assert.rejects(
      () => agent.invoke("agent-b", "fail-cap", {}),
      (err: Error) => {
        assert.match(err.message, /target agent not available|HTTP 502/);
        return true;
      }
    );
  });

  it("should pass timeout parameter to sidecar", async () => {
    // We verify it doesn't error; the mock sidecar doesn't validate timeout
    // but the request format is correct.
    const agent = new HiveAgent({
      callbackPort: 0,
      sidecarUrl: `http://127.0.0.1:${sidecarPort}`,
    });

    const result = await agent.invoke("agent-b", "echo", { text: "hi" }, "5s");
    assert.equal(result.echoed, "hi");
  });
});

// ---------------------------------------------------------------------------
// Tests: Multiple agents on different ports
// ---------------------------------------------------------------------------

describe("Multiple HiveAgent instances", () => {
  let agent1: HiveAgent;
  let agent2: HiveAgent;
  const port1 = 19200 + Math.floor(Math.random() * 1000) + 1000;
  const port2 = port1 + 1;

  before(async () => {
    agent1 = new HiveAgent({ callbackPort: port1, agentId: "agent-1" });
    agent1.capability("ping", async () => ({ from: "agent-1" }));
    agent1.run();

    agent2 = new HiveAgent({ callbackPort: port2, agentId: "agent-2" });
    agent2.capability("ping", async () => ({ from: "agent-2" }));
    agent2.run();

    await Promise.all([waitForServer(port1), waitForServer(port2)]);
  });

  after(() => {
    agent1.stop();
    agent2.stop();
  });

  it("both agents respond independently", async () => {
    const [res1, res2] = await Promise.all([
      request("POST", `http://127.0.0.1:${port1}/handle/ping`, { inputs: {} }),
      request("POST", `http://127.0.0.1:${port2}/handle/ping`, { inputs: {} }),
    ]);

    assert.equal(res1.status, 200);
    const out1 = res1.body.outputs as Record<string, unknown>;
    assert.equal(out1.from, "agent-1");

    assert.equal(res2.status, 200);
    const out2 = res2.body.outputs as Record<string, unknown>;
    assert.equal(out2.from, "agent-2");
  });
});
