#!/usr/bin/env bash
# Hive CI Pipeline - Test Runner Agent
# Runs test commands and returns structured results.
# No LLM required. Requires python3.
set -euo pipefail

if ! command -v python3 &>/dev/null; then
    echo "[test-runner] ERROR: python3 is required" >&2
    exit 1
fi

python3 -c "
import http.server, json, subprocess, shlex, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9201'))
WORKSPACE = os.path.realpath(os.environ.get('HIVE_WORKSPACE', os.getcwd()))

def safe_cwd(repo_path):
    \"\"\"Resolve repo_path and ensure it stays within the workspace.\"\"\"
    resolved = os.path.realpath(os.path.join(WORKSPACE, repo_path))
    if not resolved.startswith(WORKSPACE):
        return None
    return resolved

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[test-runner] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/run-tests'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            repo_path = inputs.get('repo_path', '.')
            test_command = inputs.get('test_command', 'echo no test command')

            cwd = safe_cwd(repo_path)
            if cwd is None:
                resp = json.dumps({'outputs': {
                    'passed': 0, 'failed': 1,
                    'output': f'repo_path outside workspace: {repo_path}',
                    'success': False
                }})
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', len(resp))
                self.end_headers()
                self.wfile.write(resp.encode())
                return

            try:
                result = subprocess.run(
                    shlex.split(test_command), capture_output=True, text=True,
                    cwd=cwd, timeout=120
                )
                output = (result.stdout + result.stderr)[:65536]
                exit_code = result.returncode
            except subprocess.TimeoutExpired:
                output = 'Test command timed out after 120s'
                exit_code = 1
            except Exception as e:
                output = str(e)
                exit_code = 1

            passed = output.count('PASS') + output.count('ok ')
            failed = output.count('FAIL')
            success = exit_code == 0

            resp = json.dumps({'outputs': {
                'passed': passed, 'failed': failed,
                'output': output, 'success': success
            }})
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
print(f'[test-runner] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
