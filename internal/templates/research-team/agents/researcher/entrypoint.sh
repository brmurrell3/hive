#!/usr/bin/env bash
# Hive Research Team - Researcher Agent (Lead)
# Researches topics and orchestrates the synthesizer for formatted output.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9200}"
SIDECAR_URL="${HIVE_SIDECAR_URL:-http://127.0.0.1:9100}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal, time

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9200'))
SIDECAR_URL = os.environ.get('HIVE_SIDECAR_URL', 'http://127.0.0.1:9100')
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def research_topic(topic, depth='basic'):
    \"\"\"Research a topic using Claude API or return mock results.\"\"\"
    if API_KEY:
        try:
            prompt = f'Research the following topic and provide detailed findings. Depth: {depth}. Topic: {topic}\n\nReturn JSON with keys: findings (string with detailed findings), sources_count (int), depth (string).'
            data = json.dumps({
                'model': 'claude-sonnet-4-5-20250514',
                'max_tokens': 2048 if depth == 'deep' else 1024,
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
                    return {'findings': text, 'sources_count': 1, 'depth': depth}
        except Exception as e:
            print(f'[researcher] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    if depth == 'deep':
        findings = (
            f'Deep research findings on \"{topic}\":\n'
            f'1. Historical context: The topic has evolved significantly over the past decade.\n'
            f'2. Current state: Recent developments show promising advances in key areas.\n'
            f'3. Key players: Multiple organizations are actively contributing to progress.\n'
            f'4. Technical details: Core mechanisms involve several interconnected systems.\n'
            f'5. Future outlook: Experts predict continued growth and innovation.\n'
            f'6. Challenges: Scalability and adoption remain significant hurdles.\n'
            f'7. Related fields: Cross-pollination with adjacent domains is accelerating.'
        )
        sources = 12
    else:
        findings = (
            f'Basic research findings on \"{topic}\":\n'
            f'1. Overview: The topic covers an important and evolving area.\n'
            f'2. Key points: Several notable developments have occurred recently.\n'
            f'3. Summary: Further investigation recommended for deeper insights.'
        )
        sources = 5

    return {'findings': findings, 'sources_count': sources, 'depth': depth}

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

def orchestrate_research(payload):
    \"\"\"Run the research pipeline: research topic, then synthesize findings.\"\"\"
    start = time.time()
    topic = payload.get('topic', 'general knowledge')
    depth = payload.get('depth', 'basic')
    fmt = payload.get('format', 'summary')

    # Step 1: Research the topic
    research_result = research_topic(topic, depth)
    findings = research_result.get('findings', '')

    # Step 2: Send findings to synthesizer for formatting
    synth_result = invoke_remote('synthesize-findings', 'synthesizer',
        {'findings': findings, 'format': fmt})

    duration = time.time() - start

    synth_outputs = synth_result.get('outputs', synth_result)

    report = {
        'pipeline': 'research-team',
        'topic': topic,
        'depth': depth,
        'duration_seconds': round(duration, 2),
        'research': research_result,
        'synthesis': synth_outputs,
    }

    print(json.dumps(report, indent=2), file=sys.stderr)
    return report

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[researcher] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        if length > 1048576:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(length)) if length > 0 else {}
        inputs = body.get('inputs', {})

        if self.path.startswith('/handle/research-topic'):
            result = research_topic(inputs.get('topic', ''), inputs.get('depth', 'basic'))
            resp = json.dumps({'outputs': result})
            self.send_response(200)
        elif self.path.startswith('/handle/orchestrate'):
            result = orchestrate_research(inputs)
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
print(f'[researcher] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
