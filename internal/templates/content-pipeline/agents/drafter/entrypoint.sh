#!/usr/bin/env bash
# Hive Content Pipeline - Drafter Agent (Lead)
# Drafts content and orchestrates editor and fact-checker.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9200}"
SIDECAR_URL="${HIVE_SIDECAR_URL:-http://127.0.0.1:9100}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal, threading, time

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9200'))
SIDECAR_URL = os.environ.get('HIVE_SIDECAR_URL', 'http://127.0.0.1:9100')
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def draft_content(topic, tone='professional', length='medium'):
    \"\"\"Draft content using Claude API or return mock results.\"\"\"
    length_tokens = {'short': 512, 'medium': 1024, 'long': 2048}
    max_tokens = length_tokens.get(length, 1024)

    if API_KEY:
        try:
            prompt = f'Write a {length} {tone} article about: {topic}. Return JSON with keys: content (string), word_count (int), tone (string).'
            data = json.dumps({
                'model': 'claude-sonnet-4-5-20250514',
                'max_tokens': max_tokens,
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
                    return {'content': text, 'word_count': len(text.split()), 'tone': tone}
        except Exception as e:
            print(f'[drafter] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    if length == 'short':
        content = f'{topic.title()} is an important subject. Key developments continue to shape the landscape. Experts recommend staying informed about recent changes.'
    elif length == 'long':
        content = (
            f'{topic.title()}: A Comprehensive Overview\n\n'
            f'Introduction\n'
            f'The field of {topic} has seen remarkable growth in recent years. '
            f'This article explores the key aspects, challenges, and opportunities.\n\n'
            f'Background\n'
            f'{topic.title()} emerged as a significant area of interest due to its broad implications. '
            f'Understanding its foundations is essential for grasping current developments.\n\n'
            f'Current State\n'
            f'Today, {topic} is characterized by rapid innovation and increasing adoption. '
            f'Multiple stakeholders are contributing to its advancement.\n\n'
            f'Challenges\n'
            f'Despite progress, several challenges remain including scalability, '
            f'standardization, and accessibility.\n\n'
            f'Future Outlook\n'
            f'The trajectory for {topic} appears promising, with continued investment '
            f'and growing interest from both industry and academia.\n\n'
            f'Conclusion\n'
            f'As {topic} continues to evolve, staying informed and engaged will be '
            f'critical for professionals and organizations alike.'
        )
    else:
        content = (
            f'{topic.title()}: Key Insights\n\n'
            f'The landscape of {topic} continues to evolve rapidly. Recent developments '
            f'have brought both opportunities and challenges to the forefront.\n\n'
            f'Key areas to watch include emerging trends, best practices, and the '
            f'growing impact on various industries. Experts suggest that understanding '
            f'these dynamics is crucial for informed decision-making.\n\n'
            f'In summary, {topic} remains a vital area worthy of continued attention '
            f'and investment.'
        )

    return {'content': content, 'word_count': len(content.split()), 'tone': tone}

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

def orchestrate_content(payload):
    \"\"\"Run the content pipeline: draft, then edit + fact-check in parallel.\"\"\"
    start = time.time()
    topic = payload.get('topic', 'general topic')
    tone = payload.get('tone', 'professional')
    length = payload.get('length', 'medium')
    style = payload.get('style', 'ap')

    # Step 1: Draft the content
    draft_result = draft_content(topic, tone, length)
    content = draft_result.get('content', '')

    # Step 2: Edit and fact-check in parallel
    results = {}
    errors = {}

    def run_edit():
        try:
            results['edit'] = invoke_remote('edit-content', 'editor',
                {'content': content, 'style': style})
        except Exception as e:
            errors['edit'] = str(e)

    def run_fact_check():
        try:
            results['fact_check'] = invoke_remote('check-facts', 'fact-checker',
                {'content': content})
        except Exception as e:
            errors['fact_check'] = str(e)

    threads = [
        threading.Thread(target=run_edit),
        threading.Thread(target=run_fact_check),
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)

    duration = time.time() - start

    report = {
        'pipeline': 'content-pipeline',
        'topic': topic,
        'duration_seconds': round(duration, 2),
        'draft': draft_result,
        'edit': results.get('edit', {'error': errors.get('edit', 'not completed')}),
        'fact_check': results.get('fact_check', {'error': errors.get('fact_check', 'not completed')}),
    }
    if errors:
        report['errors'] = errors

    print(json.dumps(report, indent=2), file=sys.stderr)
    return report

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[drafter] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        if length > 1048576:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(length)) if length > 0 else {}
        inputs = body.get('inputs', {})

        if self.path.startswith('/handle/draft-content'):
            result = draft_content(inputs.get('topic', ''), inputs.get('tone', 'professional'), inputs.get('length', 'medium'))
            resp = json.dumps({'outputs': result})
            self.send_response(200)
        elif self.path.startswith('/handle/orchestrate'):
            result = orchestrate_content(inputs)
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
print(f'[drafter] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
