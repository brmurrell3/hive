#!/usr/bin/env bash
# Hive CI Pipeline - Code Reviewer Agent (Lead)
# Reviews code and orchestrates the CI pipeline when triggered.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9200}"
SIDECAR_URL="${HIVE_SIDECAR_URL:-http://127.0.0.1:9100}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal, threading, time

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

def review_code(file_path, diff=None):
    \"\"\"Review code using Claude API or return mock results.\"\"\"
    content = diff or ''
    if not content and file_path:
        resolved = safe_path(file_path)
        if resolved is None:
            content = f'Path rejected (outside workspace): {file_path}'
        else:
            try:
                with open(resolved) as f:
                    content = f.read()[:8000]
            except Exception as e:
                content = f'Could not read {file_path}: {e}'

    if API_KEY:
        try:
            data = json.dumps({
                'model': 'claude-sonnet-4-5-20250514',
                'max_tokens': 1024,
                'messages': [{'role': 'user', 'content': f'Review this code concisely. List bugs, style issues, improvements. Return JSON with keys: review (string), severity (info/warning/critical), findings_count (int).\\n\\n{content}'}]
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
                    return {'review': text, 'severity': 'info', 'findings_count': 1}
        except Exception as e:
            print(f'[code-reviewer] API call failed: {e}', file=sys.stderr)

    # Mock response when no API key
    return {
        'review': f'Mock review of {file_path or \"diff\"}: Code looks generally well-structured. Consider adding error handling for edge cases. Variable naming is consistent.',
        'severity': 'info',
        'findings_count': 2
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

def orchestrate_pipeline(payload):
    \"\"\"Run the full CI pipeline: review + tests + security scan in parallel.\"\"\"
    start = time.time()
    repo_path = payload.get('repo_path', '.')
    test_command = payload.get('test_command', 'echo no tests configured')
    file_path = payload.get('file_path', repo_path)

    results = {}
    errors = {}

    def run_review():
        try:
            results['review'] = review_code(file_path)
        except Exception as e:
            errors['review'] = str(e)

    def run_tests():
        try:
            results['tests'] = invoke_remote('run-tests', 'test-runner',
                {'repo_path': repo_path, 'test_command': test_command})
        except Exception as e:
            errors['tests'] = str(e)

    def run_security():
        try:
            results['security'] = invoke_remote('scan-security', 'security-scanner',
                {'file_path': file_path})
        except Exception as e:
            errors['security'] = str(e)

    # Run all three in parallel
    threads = [
        threading.Thread(target=run_review),
        threading.Thread(target=run_tests),
        threading.Thread(target=run_security),
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)

    duration = time.time() - start

    # Determine overall pass/fail
    test_success = True
    if 'tests' in results:
        outputs = results['tests'].get('outputs', results['tests'])
        test_success = outputs.get('success', True)

    overall = 'pass' if test_success and 'tests' not in errors else 'fail'

    report = {
        'pipeline': 'ci-pipeline',
        'overall': overall,
        'duration_seconds': round(duration, 2),
        'review': results.get('review', {'error': errors.get('review', 'not completed')}),
        'tests': results.get('tests', {'error': errors.get('tests', 'not completed')}),
        'security': results.get('security', {'error': errors.get('security', 'not completed')}),
    }
    if errors:
        report['errors'] = errors

    print(json.dumps(report, indent=2), file=sys.stderr)
    return report

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[code-reviewer] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = json.loads(self.rfile.read(length)) if length > 0 else {}
        inputs = body.get('inputs', {})

        if self.path.startswith('/handle/review-code'):
            result = review_code(inputs.get('file_path', ''), inputs.get('diff'))
            resp = json.dumps({'outputs': result})
            self.send_response(200)
        elif self.path.startswith('/handle/orchestrate'):
            result = orchestrate_pipeline(inputs)
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
print(f'[code-reviewer] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
