# Hive TypeScript SDK

A zero-dependency TypeScript SDK for building [Hive](https://github.com/brmurrell3/hive) agents. Requires Node.js 18+.

## Installation

```bash
cd sdk/typescript
npm install
npm run build
```

## Quick Start

```typescript
import { HiveAgent } from "@hive/sdk";

const agent = new HiveAgent();

agent.capability("greet", async (inputs) => {
  const name = inputs.name as string;
  const greeting = (inputs.greeting as string) || "Hello";
  return { message: `${greeting}, ${name}!` };
});

agent.run();
```

Set the required environment variables before running:

```bash
export HIVE_CALLBACK_PORT=9200
export HIVE_AGENT_ID=my-agent
export HIVE_TEAM_ID=my-team
export HIVE_SIDECAR_URL=http://127.0.0.1:9100
npx tsx my_agent.ts
```

## Environment Variables

| Variable             | Required | Description                               |
|----------------------|----------|-------------------------------------------|
| `HIVE_CALLBACK_PORT` | No       | Port the HTTP server listens on (default: 9200) |
| `HIVE_AGENT_ID`      | No       | Agent identifier                          |
| `HIVE_TEAM_ID`       | No       | Team identifier                           |
| `HIVE_SIDECAR_URL`   | No       | Sidecar API URL (default: http://127.0.0.1:9100) |
| `HIVE_WORKSPACE`     | No       | Workspace directory                       |

## Capabilities

Register capabilities with `agent.capability(name, handler)`. The handler:

- Receives an `inputs` map with the capability's input values
- Returns a `Record<string, unknown>` that is automatically wrapped as `{"outputs": {...}}`
- Thrown errors are caught and returned as structured error responses

```typescript
agent.capability("add", async (inputs) => {
  const a = inputs.a as number;
  const b = inputs.b as number;
  return { sum: a + b };
});

agent.capability("divide", async (inputs) => {
  const num = inputs.numerator as number;
  const den = inputs.denominator as number;
  if (den === 0) throw new Error("cannot divide by zero");
  return { result: num / den };
});
```

## Remote Invocation

Call capabilities on other agents via the sidecar:

```typescript
const result = await agent.invoke("other-agent", "process-data", {
  data: "hello",
});
console.log(result); // { status: "ok", outputs: { processed: "HELLO" } }
```

## HTTP Protocol

The SDK implements the Hive agent callback protocol:

| Method | Path                        | Description         |
|--------|-----------------------------|---------------------|
| GET    | `/health`                   | Health check        |
| POST   | `/handle/{capability_name}` | Invoke a capability |

### Request format (POST /handle/{name})

```json
{
  "inputs": {
    "key": "value"
  }
}
```

### Success response (HTTP 200)

```json
{
  "outputs": {
    "key": "value"
  }
}
```

### Error response (HTTP 500)

```json
{
  "error": {
    "code": "CAPABILITY_FAILED",
    "message": "description of the error"
  }
}
```

## Testing

```bash
cd sdk/typescript
npm test
```

## License

Apache-2.0
