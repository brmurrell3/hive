#!/usr/bin/env bash
# Hive CI Pipeline - Test Runner Agent
# Runs test commands and returns structured results.
# No LLM required.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9201}"

handle_request() {
    local method path body
    read -r method path _
    method=$(echo "$method" | tr -d '\r')
    path=$(echo "$path" | tr -d '\r')

    # Read headers until empty line
    local content_length=0
    while IFS= read -r header; do
        header=$(echo "$header" | tr -d '\r')
        [ -z "$header" ] && break
        if echo "$header" | grep -qi "^content-length:"; then
            content_length=$(echo "$header" | sed 's/[^0-9]//g')
        fi
    done

    # Read body
    body=""
    if [ "$content_length" -gt 0 ] 2>/dev/null; then
        body=$(dd bs=1 count="$content_length" 2>/dev/null)
    fi

    if [ "$method" = "POST" ] && echo "$path" | grep -q "^/handle/run-tests"; then
        # Extract inputs
        local repo_path test_command
        repo_path=$(echo "$body" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('inputs',{}).get('repo_path','.'))" 2>/dev/null || echo ".")
        test_command=$(echo "$body" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('inputs',{}).get('test_command','echo no test command'))" 2>/dev/null || echo "echo no test command")

        # Run the test command
        local output exit_code
        output=$(cd "$repo_path" 2>/dev/null && eval "$test_command" 2>&1) && exit_code=0 || exit_code=$?

        # Truncate output to 64KB
        output=$(echo "$output" | head -c 65536)

        # Count pass/fail (best-effort parsing)
        local passed=0 failed=0 success="true"
        if echo "$output" | grep -q "FAIL"; then
            success="false"
            failed=$(echo "$output" | grep -c "FAIL" || true)
        fi
        if echo "$output" | grep -q "PASS\|ok "; then
            passed=$(echo "$output" | grep -c "PASS\|ok " || true)
        fi
        if [ "$exit_code" -ne 0 ]; then
            success="false"
        fi

        # Escape output for JSON
        local json_output
        json_output=$(echo "$output" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))" 2>/dev/null || echo '""')

        local response_body
        response_body=$(cat <<ENDJSON
{"outputs":{"passed":$passed,"failed":$failed,"output":$json_output,"success":$success}}
ENDJSON
)
        local len=${#response_body}
        printf "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s" "$len" "$response_body"
    elif [ "$method" = "GET" ] && [ "$path" = "/health" ]; then
        local resp='{"status":"healthy"}'
        printf "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s" "${#resp}" "$resp"
    else
        local resp='{"error":"not found"}'
        printf "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s" "${#resp}" "$resp"
    fi
}

echo "[test-runner] Starting on port $PORT" >&2

# Check for socat or python3 for HTTP server
if command -v python3 &>/dev/null; then
    python3 -c "
import http.server, json, subprocess, os, sys, signal

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9201'))

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[test-runner] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/run-tests'):
            length = int(self.headers.get('Content-Length', 0))
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            repo_path = inputs.get('repo_path', '.')
            test_command = inputs.get('test_command', 'echo no test command')

            try:
                result = subprocess.run(
                    test_command, shell=True, capture_output=True, text=True,
                    cwd=repo_path, timeout=120
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
else
    echo "[test-runner] ERROR: python3 is required" >&2
    exit 1
fi
