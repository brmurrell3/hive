# Monitor Template

A two-agent monitoring system that watches targets and sends alerts when issues are detected.

## Agents

| Agent | Capability | Description |
|---|---|---|
| watcher (lead) | watch-target | Monitors targets via HTTP, port, or process checks |
| alerter | send-alert | Sends alerts with configurable severity and channel |

## How It Works

1. A trigger is sent to the `monitor` team
2. The **watcher** (lead agent) receives the trigger and checks the target
3. If issues are detected, it invokes **alerter** to send notifications
4. Results are aggregated into a monitoring report

## Quick Start

```bash
hivectl init --template monitor ./my-monitor
export ANTHROPIC_API_KEY=sk-...  # optional, mock results without it
hivectl dev --cluster-root ./my-monitor
```

Then trigger a check:

```bash
hivectl trigger --cluster-root ./my-monitor --team monitor \
  --payload '{"target": "https://example.com", "check_type": "http"}'
```

## Customization

- Edit `agents/*/manifest.yaml` to change capabilities
- Edit `agents/*/entrypoint.sh` to modify agent logic
- Adjust `check_type` to `http`, `port`, or `process`
- Adjust `severity` to `info`, `warning`, or `critical` for alerts
- Adjust `channel` to `log` or `webhook` for alert delivery
- Set `ANTHROPIC_API_KEY` for AI-powered analysis of check results
