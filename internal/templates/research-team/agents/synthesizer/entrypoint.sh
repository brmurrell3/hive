#!/usr/bin/env bash
# Hive Research Team - Synthesizer Agent
# Formats research findings into structured output.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9201}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9201'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def synthesize_findings(findings, fmt='summary'):
    \"\"\"Synthesize findings using Claude API or return mock results.\"\"\"
    if API_KEY:
        try:
            prompt = f'Synthesize the following research findings into a {fmt}. Return JSON with keys: synthesis (string), format (string), word_count (int).\n\nFindings:\n{findings}'
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
                    return {'synthesis': text, 'format': fmt, 'word_count': len(text.split())}
        except Exception as e:
            print(f'[synthesizer] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    if fmt == 'report':
        synthesis = (
            f'RESEARCH REPORT\n'
            f'===============\n\n'
            f'Executive Summary:\n'
            f'This report synthesizes key findings from the research phase.\n\n'
            f'Details:\n{findings}\n\n'
            f'Conclusions:\n'
            f'The findings indicate several important trends that merit further investigation.\n\n'
            f'Recommendations:\n'
            f'1. Continue monitoring developments in this area.\n'
            f'2. Consider deeper investigation into specific sub-topics.'
        )
    elif fmt == 'briefing':
        synthesis = (
            f'BRIEFING DOCUMENT\n'
            f'-----------------\n'
            f'Key Takeaways:\n'
            f'- Research completed successfully\n'
            f'- Multiple relevant findings identified\n\n'
            f'Core Findings:\n{findings}\n\n'
            f'Action Items:\n'
            f'- Review findings with stakeholders\n'
            f'- Plan follow-up research if needed'
        )
    else:
        synthesis = (
            f'Summary: Research yielded several key insights. {findings[:200]}... '
            f'Further analysis recommended for actionable conclusions.'
        )

    return {
        'synthesis': synthesis,
        'format': fmt,
        'word_count': len(synthesis.split())
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[synthesizer] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/synthesize-findings'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = synthesize_findings(inputs.get('findings', ''), inputs.get('format', 'summary'))
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
print(f'[synthesizer] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
