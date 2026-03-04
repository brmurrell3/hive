#!/usr/bin/env bash
# Hive Data Processor - Transformer Agent
# Applies transformations to data records.
# Uses ANTHROPIC_API_KEY if available, otherwise applies basic transformations.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9201}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9201'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def transform_data(data_str, operations='passthrough'):
    \"\"\"Transform data using Claude API or apply basic transformations.\"\"\"
    try:
        records = json.loads(data_str)
        if not isinstance(records, list):
            records = [records]
    except (json.JSONDecodeError, TypeError):
        records = [{'raw': data_str}]

    ops = [op.strip() for op in operations.split(',')]

    if API_KEY and operations != 'passthrough':
        try:
            prompt = f'Apply these transformations to the data: {operations}. Data: {json.dumps(records[:100])}. Return JSON with keys: data (transformed records array), records_count (int), operations_applied (string).'
            data = json.dumps({
                'model': 'claude-sonnet-4-5-20250514',
                'max_tokens': 1024,
                'messages': [{'role': 'user', 'content': prompt}]
            }).encode()
            req = urllib.request.Request('https://api.anthropic.com/v1/messages',
                data=data,
                headers={'Content-Type': 'application/json', 'x-api-key': API_KEY, 'anthropic-version': '2023-06-01'})
            with urllib.request.urlopen(req, timeout=30) as resp:
                result = json.loads(resp.read())
                text = result['content'][0]['text']
                try:
                    parsed = json.loads(text)
                    if 'data' in parsed and isinstance(parsed['data'], list):
                        parsed['data'] = json.dumps(parsed['data'])
                    return parsed
                except json.JSONDecodeError:
                    pass
        except Exception as e:
            print(f'[transformer] API call failed: {e}', file=sys.stderr)

    # Apply basic mock transformations
    applied = []

    if 'normalize' in ops:
        for record in records:
            for key in list(record.keys()):
                if isinstance(record[key], str):
                    record[key] = record[key].strip().lower()
        applied.append('normalize')

    if 'deduplicate' in ops:
        seen = set()
        unique = []
        for record in records:
            key = json.dumps(record, sort_keys=True)
            if key not in seen:
                seen.add(key)
                unique.append(record)
        records = unique
        applied.append('deduplicate')

    if 'filter' in ops:
        records = [r for r in records if any(v for v in r.values() if v)]
        applied.append('filter')

    if 'passthrough' in ops or not applied:
        applied.append('passthrough')

    return {
        'data': json.dumps(records),
        'records_count': len(records),
        'operations_applied': ','.join(applied)
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[transformer] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/transform-data'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = transform_data(inputs.get('data', '[]'), inputs.get('operations', 'passthrough'))
            resp = json.dumps({'outputs': result})
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', len(resp))
            self.end_headers()
            self.wfile.write(resp.encode())
        else:
            self.send_error(404)

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
print(f'[transformer] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
