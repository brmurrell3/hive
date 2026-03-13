#!/usr/bin/env bash
# Hive Content Pipeline - Editor Agent
# Edits content for style consistency and clarity.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9201}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9201'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def edit_content(content, style='ap'):
    \"\"\"Edit content using Claude API or return mock results.\"\"\"
    if API_KEY:
        try:
            prompt = f'Edit the following content according to {style.upper()} style guide. Fix grammar, punctuation, and style issues. Return JSON with keys: edited_content (string), changes_made (int), style (string).\n\nContent:\n{content}'
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
                    return {'edited_content': text, 'changes_made': 1, 'style': style}
        except Exception as e:
            print(f'[editor] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    style_notes = {
        'ap': 'Applied AP style: shortened paragraphs, active voice, concise headlines.',
        'chicago': 'Applied Chicago style: formal tone, serial commas, detailed attribution.',
        'casual': 'Applied casual style: conversational tone, simplified language, contractions.',
    }
    note = style_notes.get(style, style_notes['ap'])

    return {
        'edited_content': f'{content}\n\n[Editor note: {note}]',
        'changes_made': 3,
        'style': style
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[editor] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/edit-content'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = edit_content(inputs.get('content', ''), inputs.get('style', 'ap'))
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
print(f'[editor] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
