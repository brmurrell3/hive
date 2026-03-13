# Content Pipeline Template

A three-agent content creation pipeline that drafts, edits, and fact-checks content.

## Agents

| Agent | Capability | Description |
|---|---|---|
| drafter (lead) | draft-content | Drafts content with configurable tone and length |
| editor | edit-content | Edits content for style consistency (AP, Chicago, casual) |
| fact-checker | check-facts | Verifies factual claims in content |

## How It Works

1. A trigger is sent to the `content-pipeline` team
2. The **drafter** (lead agent) receives the trigger and creates initial content
3. It invokes **editor** and **fact-checker** in parallel
4. Results are aggregated into a final content package

## Quick Start

```bash
hivectl init --template content-pipeline ./my-content
export ANTHROPIC_API_KEY=sk-...  # optional, mock results without it
hivectl dev --cluster-root ./my-content
```

Then trigger content creation:

```bash
hivectl trigger --cluster-root ./my-content --team content-pipeline \
  --payload '{"topic": "cloud computing trends", "tone": "professional", "length": "medium"}'
```

## Customization

- Edit `agents/*/manifest.yaml` to change capabilities
- Edit `agents/*/entrypoint.sh` to modify agent logic
- Adjust `tone` to `professional`, `casual`, or `technical`
- Adjust `length` to `short`, `medium`, or `long`
- Adjust `style` to `ap`, `chicago`, or `casual` for the editor
- Set `ANTHROPIC_API_KEY` for real AI-powered content creation
