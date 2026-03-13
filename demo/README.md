# CI Pipeline Template

A three-agent CI pipeline that reviews code, runs tests, and scans for security vulnerabilities.

## Agents

| Agent | Capability | Description |
|---|---|---|
| code-reviewer (lead) | review-code | Reviews code for bugs, style, and improvements |
| test-runner | run-tests | Executes test suites and parses results |
| security-scanner | scan-security | Scans for security vulnerabilities |

## How It Works

1. A trigger is sent to the `ci-pipeline` team
2. The **code-reviewer** (lead agent) receives the trigger
3. It invokes **test-runner** and **security-scanner** in parallel
4. Results are aggregated into a pipeline report

## Quick Start

```bash
hivectl init --template ci-pipeline ./demo
export ANTHROPIC_API_KEY=sk-...  # optional, mock results without it
hivectl dev --cluster-root ./demo
```

Then trigger the pipeline:

```bash
hivectl trigger --cluster-root ./demo --team ci-pipeline \
  --payload '{"repo_path": ".", "test_command": "go test ./..."}'
```

## Customization

- Edit `agents/*/manifest.yaml` to change capabilities
- Edit `agents/*/entrypoint.sh` to modify agent logic
- The test-runner works with any test command (Go, Python, Node, etc.)
- Set `ANTHROPIC_API_KEY` for real AI-powered code review and security scanning
