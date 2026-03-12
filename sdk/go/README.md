# Hive Go SDK

A lightweight Go SDK for building [Hive](https://github.com/brmurrell3/hive) agents.

## Installation

```bash
go get github.com/brmurrell3/hive/sdk/go/hive
```

## Quick Start

```go
package main

import (
	"context"
	"github.com/brmurrell3/hive/sdk/go/hive"
)

func main() {
	agent := hive.NewAgent()

	agent.HandleCapability("greet", func(inputs map[string]any) (map[string]any, error) {
		name := inputs["name"].(string)
		return map[string]any{"message": "Hello, " + name + "!"}, nil
	})

	agent.Run(context.Background())
}
```

Set the required environment variables before running:

```bash
export HIVE_CALLBACK_PORT=9200
export HIVE_AGENT_ID=my-agent
export HIVE_TEAM_ID=my-team
export HIVE_SIDECAR_URL=http://127.0.0.1:9100
go run main.go
```

## Environment Variables

| Variable             | Required | Description                                       |
|----------------------|----------|---------------------------------------------------|
| `HIVE_CALLBACK_PORT` | No       | Port the HTTP server listens on (default: 9200)   |
| `HIVE_AGENT_ID`      | No       | Agent identifier                                  |
| `HIVE_TEAM_ID`       | No       | Team identifier                                   |
| `HIVE_SIDECAR_URL`   | No       | Sidecar API URL (default: http://127.0.0.1:9100)  |
| `HIVE_WORKSPACE`     | No       | Workspace directory                               |

## Capabilities

Register capability handlers with `agent.HandleCapability(name, handler)`. The handler:

- Receives an `inputs` map with the capability's input values
- Returns an `outputs` map that is automatically wrapped as `{"outputs": {...}}`
- Returning a non-nil error produces a structured error response

```go
agent.HandleCapability("add", func(inputs map[string]any) (map[string]any, error) {
    a := inputs["a"].(float64)
    b := inputs["b"].(float64)
    return map[string]any{"sum": a + b}, nil
})

agent.HandleCapability("divide", func(inputs map[string]any) (map[string]any, error) {
    num := inputs["numerator"].(float64)
    den := inputs["denominator"].(float64)
    if den == 0 {
        return nil, fmt.Errorf("cannot divide by zero")
    }
    return map[string]any{"result": num / den}, nil
})
```

## Remote Invocation

Call capabilities on other agents via the sidecar:

```go
result, err := agent.Invoke(ctx, "other-agent", "process-data", map[string]any{
    "data": "hello",
})
// result["outputs"] = map[string]any{"processed": "HELLO"}
```

## HTTP Protocol

The SDK implements the Hive agent callback protocol:

| Method | Path                        | Description         |
|--------|-----------------------------|---------------------|
| GET    | `/health`                   | Health check        |
| POST   | `/handle/{capability_name}` | Invoke a capability |

## Testing

```bash
cd sdk/go
go test ./...
```

## License

Apache-2.0
