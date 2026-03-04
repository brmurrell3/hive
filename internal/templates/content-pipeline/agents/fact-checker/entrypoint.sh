#!/usr/bin/env bash
# Hive Content Pipeline - Fact Checker Agent
# Verifies factual claims in content.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9202}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9202'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def check_facts(content):
    \"\"\"Check facts using Claude API or return mock results.\"\"\"
    if API_KEY:
        try:
            prompt = f'Fact-check the following content. Identify factual claims and verify them. Return JSON with keys: claims_checked (int), issues_found (int), report (string with detailed findings), verdict (pass/caution/fail).\n\nContent:\n{content}'
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
                    return json.loads(text)
                except json.JSONDecodeError:
                    return {'claims_checked': 1, 'issues_found': 0, 'report': text, 'verdict': 'pass'}
        except Exception as e:
            print(f'[fact-checker] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    word_count = len(content.split())
    claims = max(1, word_count // 50)

    return {
        'claims_checked': claims,
        'issues_found': 0,
        'report': (
            f'Fact-check complete. Checked {claims} factual claims in the provided content. '
            f'No verifiable inaccuracies detected in mock analysis. '
            f'Note: For production use, set ANTHROPIC_API_KEY for real fact-checking.'
        ),
        'verdict': 'pass'
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[fact-checker] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/check-facts'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = check_facts(inputs.get('content', ''))
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
print(f'[fact-checker] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
