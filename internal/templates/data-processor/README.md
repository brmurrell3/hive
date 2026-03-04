# Data Processor Template

A three-agent data processing pipeline that ingests, transforms, and validates data.

## Agents

| Agent | Capability | Description |
|---|---|---|
| ingestor (lead) | ingest-data | Ingests data from various formats (CSV, JSON, text) |
| transformer | transform-data | Applies transformations to data |
| validator | validate-data | Validates data against specified rules |

## How It Works

1. A trigger is sent to the `data-processor` team
2. The **ingestor** (lead agent) receives the trigger and parses input data
3. It invokes **transformer** to apply requested transformations
4. It then invokes **validator** to verify the transformed data
5. Results are aggregated into a processing report

## Quick Start

```bash
hivectl init --template data-processor ./my-data
export ANTHROPIC_API_KEY=sk-...  # optional, mock results without it
hivectl dev --cluster-root ./my-data
```

Then trigger data processing:

```bash
hivectl trigger --cluster-root ./my-data --team data-processor \
  --payload '{"source": "sample.csv", "format": "csv", "operations": "normalize,deduplicate", "rules": "no_nulls,unique_ids"}'
```

## Customization

- Edit `agents/*/manifest.yaml` to change capabilities
- Edit `agents/*/entrypoint.sh` to modify agent logic
- Adjust `format` to `csv`, `json`, or `text` for different input types
- Customize `operations` string for transformer behavior
- Customize `rules` string for validator checks
- Set `ANTHROPIC_API_KEY` for AI-powered data analysis and transformation
