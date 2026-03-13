#!/usr/bin/env bash
# Hive Data Processor - Validator Agent
# Validates data against specified rules.
# Uses ANTHROPIC_API_KEY if available, otherwise applies basic validation.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9202}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9202'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def validate_data(data_str, rules='basic'):
    \"\"\"Validate data using Claude API or apply basic validation rules.\"\"\"
    try:
        records = json.loads(data_str)
        if not isinstance(records, list):
            records = [records]
    except (json.JSONDecodeError, TypeError):
        records = [{'raw': data_str}]

    rule_list = [r.strip() for r in rules.split(',')]

    if API_KEY and rules != 'basic':
        try:
            prompt = f'Validate this data against these rules: {rules}. Data: {json.dumps(records[:100])}. Return JSON with keys: valid (bool), records_checked (int), violations (JSON array of objects with record_index, rule, message), violations_count (int).'
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
                    if 'violations' in parsed and isinstance(parsed['violations'], list):
                        parsed['violations'] = json.dumps(parsed['violations'])
                    return parsed
                except json.JSONDecodeError:
                    pass
        except Exception as e:
            print(f'[validator] API call failed: {e}', file=sys.stderr)

    # Apply basic mock validation rules
    violations = []

    if 'no_nulls' in rule_list:
        for i, record in enumerate(records):
            for key, val in record.items():
                if val is None or val == '' or val == 'null':
                    violations.append({
                        'record_index': i,
                        'rule': 'no_nulls',
                        'message': f'Field \"{key}\" is null or empty'
                    })

    if 'unique_ids' in rule_list:
        ids_seen = {}
        for i, record in enumerate(records):
            rid = record.get('id', record.get('ID', record.get('Id')))
            if rid is not None:
                if rid in ids_seen:
                    violations.append({
                        'record_index': i,
                        'rule': 'unique_ids',
                        'message': f'Duplicate ID \"{rid}\" (first seen at record {ids_seen[rid]})'
                    })
                else:
                    ids_seen[rid] = i

    if 'non_empty' in rule_list:
        if not records:
            violations.append({
                'record_index': -1,
                'rule': 'non_empty',
                'message': 'Dataset is empty'
            })

    if 'basic' in rule_list:
        pass  # Basic validation always passes for mock

    return {
        'valid': len(violations) == 0,
        'records_checked': len(records),
        'violations': json.dumps(violations),
        'violations_count': len(violations)
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[validator] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/validate-data'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = validate_data(inputs.get('data', '[]'), inputs.get('rules', 'basic'))
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
print(f'[validator] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
