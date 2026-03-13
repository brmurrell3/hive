# Research Team Template

A two-agent research team that investigates topics and synthesizes findings into structured reports.

## Agents

| Agent | Capability | Description |
|---|---|---|
| researcher (lead) | research-topic | Researches a topic at basic or deep depth |
| synthesizer | synthesize-findings | Formats findings into summary, report, or briefing |

## How It Works

1. A trigger is sent to the `research-team` team
2. The **researcher** (lead agent) receives the trigger
3. It researches the requested topic at the specified depth
4. It invokes **synthesizer** to format the findings
5. The final synthesized output is returned

## Quick Start

```bash
hivectl init --template research-team ./my-research
export ANTHROPIC_API_KEY=sk-...  # optional, mock results without it
hivectl dev --cluster-root ./my-research
```

Then trigger research:

```bash
hivectl trigger --cluster-root ./my-research --team research-team \
  --payload '{"topic": "quantum computing", "depth": "deep"}'
```

## Customization

- Edit `agents/*/manifest.yaml` to change capabilities
- Edit `agents/*/entrypoint.sh` to modify agent logic
- Adjust `depth` to `basic` or `deep` for different levels of research
- Adjust `format` to `summary`, `report`, or `briefing` for different output styles
- Set `ANTHROPIC_API_KEY` for real AI-powered research and synthesis
