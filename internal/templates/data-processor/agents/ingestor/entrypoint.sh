#!/usr/bin/env bash
# Hive Data Processor - Ingestor Agent (Lead)
# Ingests data and orchestrates transformer and validator.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9200}"
SIDECAR_URL="${HIVE_SIDECAR_URL:-http://127.0.0.1:9100}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal, time, csv, io

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9200'))
SIDECAR_URL = os.environ.get('HIVE_SIDECAR_URL', 'http://127.0.0.1:9100')
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')
WORKSPACE = os.path.realpath(os.environ.get('HIVE_WORKSPACE', os.getcwd()))

def safe_path(file_path):
    \"\"\"Resolve file_path and ensure it stays within the workspace.\"\"\"
    resolved = os.path.realpath(os.path.join(WORKSPACE, file_path))
    if not resolved.startswith(WORKSPACE):
        return None
    return resolved

def ingest_data(source, fmt='json'):
    \"\"\"Ingest data from source. Tries to read as file first, then as inline data.\"\"\"
    raw = source
    resolved = safe_path(source)
    if resolved and os.path.isfile(resolved):
        try:
            with open(resolved) as f:
                raw = f.read()[:65536]
        except Exception as e:
            raw = source

    records = []
    if fmt == 'csv':
        try:
            reader = csv.DictReader(io.StringIO(raw))
            for row in reader:
                records.append(dict(row))
        except Exception:
            records = [{'raw': line} for line in raw.strip().split('\\n') if line.strip()]
    elif fmt == 'json':
        try:
            parsed = json.loads(raw)
            if isinstance(parsed, list):
                records = parsed
            else:
                records = [parsed]
        except json.JSONDecodeError:
            records = [{'raw': raw}]
    else:
        records = [{'line': line.strip()} for line in raw.strip().split('\\n') if line.strip()]

    if not records:
        records = [
            {'id': '1', 'name': 'sample_a', 'value': '100'},
            {'id': '2', 'name': 'sample_b', 'value': '200'},
            {'id': '3', 'name': 'sample_c', 'value': '150'},
        ]

    return {
        'data': json.dumps(records),
        'records_count': len(records),
        'format': fmt
    }

def invoke_remote(capability, target, inputs, timeout='30s'):
    \"\"\"Invoke a capability on a remote agent via the sidecar.\"\"\"
    data = json.dumps({'target': target, 'inputs': inputs, 'timeout': timeout}).encode()
    req = urllib.request.Request(
        f'{SIDECAR_URL}/capabilities/{capability}/invoke-remote',
        data=data,
        headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return json.loads(resp.read())
    except Exception as e:
        return {'status': 'error', 'error': {'code': 'INVOCATION_FAILED', 'message': str(e)}}

def orchestrate_processing(payload):
    \"\"\"Run the data pipeline: ingest, transform, then validate.\"\"\"
    start = time.time()
    source = payload.get('source', '')
    fmt = payload.get('format', 'json')
    operations = payload.get('operations', 'passthrough')
    rules = payload.get('rules', 'basic')

    # Step 1: Ingest the data
    ingest_result = ingest_data(source, fmt)
    data_str = ingest_result.get('data', '[]')

    # Step 2: Transform the data
    transform_result = invoke_remote('transform-data', 'transformer',
        {'data': data_str, 'operations': operations})

    transformed = data_str
    transform_outputs = transform_result.get('outputs', transform_result)
    if 'data' in transform_outputs:
        transformed = transform_outputs['data']

    # Step 3: Validate the transformed data
    validate_result = invoke_remote('validate-data', 'validator',
        {'data': transformed, 'rules': rules})

    duration = time.time() - start

    validate_outputs = validate_result.get('outputs', validate_result)

    report = {
        'pipeline': 'data-processor',
        'duration_seconds': round(duration, 2),
        'ingest': ingest_result,
        'transform': transform_outputs,
        'validate': validate_outputs,
        'overall': validate_outputs.get('valid', True),
    }

    print(json.dumps(report, indent=2), file=sys.stderr)
    return report

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[ingestor] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        if length > 1048576:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(length)) if length > 0 else {}
        inputs = body.get('inputs', {})

        if self.path.startswith('/handle/ingest-data'):
            result = ingest_data(inputs.get('source', ''), inputs.get('format', 'json'))
            resp = json.dumps({'outputs': result})
            self.send_response(200)
        elif self.path.startswith('/handle/orchestrate'):
            result = orchestrate_processing(inputs)
            resp = json.dumps({'outputs': result})
            self.send_response(200)
        else:
            resp = json.dumps({'error': 'not found'})
            self.send_response(404)

        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', len(resp))
        self.end_headers()
        self.wfile.write(resp.encode())

    def do_GET(self):
        if self.path == '/health':
            resp = json.dumps({'status': 'healthy'})
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', len(resp))
            self.end_headers()
            self.wfile.write(resp.encode())
        else:
            self.send_error(404)

signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
http.server.HTTPServer.allow_reuse_address = True
server = http.server.HTTPServer(('127.0.0.1', PORT), Handler)
print(f'[ingestor] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
