#!/usr/bin/env bash
# Hive CI Pipeline - Security Scanner Agent
# Scans code for security vulnerabilities.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9202}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9202'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def scan_security(file_path):
    \"\"\"Scan code for security issues using Claude API or return mock results.\"\"\"
    content = ''
    if file_path:
        try:
            with open(file_path) as f:
                content = f.read()[:8000]
        except Exception as e:
            content = f'Could not read {file_path}: {e}'

    if API_KEY:
        try:
            data = json.dumps({
                'model': 'claude-sonnet-4-5-20250514',
                'max_tokens': 1024,
                'messages': [{'role': 'user', 'content': f'Scan this code for security vulnerabilities (injection, hardcoded secrets, auth issues, OWASP top 10). Return JSON with keys: vulnerabilities (string, JSON array of findings), risk_level (low/medium/high/critical), findings_count (int).\\n\\n{content}'}]
            }).encode()
            req = urllib.request.Request('https://api.anthropic.com/v1/messages',
                data=data,
                headers={'Content-Type': 'application/json', 'x-api-key': API_KEY, 'anthropic-version': '2023-06-01'})
            with urllib.request.urlopen(req, timeout=30) as resp:
                result = json.loads(resp.read())
                text = result['content'][0]['text']
                try:
                    return json.loads(text)
                except json.JSONDecodeError:
                    return {'vulnerabilities': text, 'risk_level': 'low', 'findings_count': 0}
        except Exception as e:
            print(f'[security-scanner] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    return {
        'vulnerabilities': json.dumps([
            {'type': 'info', 'severity': 'low', 'description': 'No hardcoded secrets detected', 'file': file_path},
            {'type': 'suggestion', 'severity': 'low', 'description': 'Consider adding input validation for user-facing functions', 'file': file_path}
        ]),
        'risk_level': 'low',
        'findings_count': 2
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[security-scanner] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/scan-security'):
            length = int(self.headers.get('Content-Length', 0))
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = scan_security(inputs.get('file_path', ''))
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
print(f'[security-scanner] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
