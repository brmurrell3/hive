#!/usr/bin/env bash
# Hive Monitor - Watcher Agent (Lead)
# Monitors targets and orchestrates alerter when issues are detected.
# Uses ANTHROPIC_API_KEY if available, otherwise returns mock results.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9200}"
SIDECAR_URL="${HIVE_SIDECAR_URL:-http://127.0.0.1:9100}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal, time, socket, subprocess

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9200'))
SIDECAR_URL = os.environ.get('HIVE_SIDECAR_URL', 'http://127.0.0.1:9100')
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def watch_target(target, check_type='http'):
    \"\"\"Check a target and return status.\"\"\"
    start = time.time()

    if check_type == 'http':
        try:
            req = urllib.request.Request(target, method='GET')
            with urllib.request.urlopen(req, timeout=10) as resp:
                elapsed = int((time.time() - start) * 1000)
                code = resp.getcode()
                if code < 400:
                    status = 'up'
                else:
                    status = 'degraded'
                return {
                    'status': status,
                    'response_time_ms': elapsed,
                    'details': f'HTTP {code} from {target} in {elapsed}ms'
                }
        except urllib.error.HTTPError as e:
            elapsed = int((time.time() - start) * 1000)
            return {
                'status': 'degraded' if e.code < 500 else 'down',
                'response_time_ms': elapsed,
                'details': f'HTTP {e.code} from {target}: {e.reason}'
            }
        except Exception as e:
            elapsed = int((time.time() - start) * 1000)
            return {
                'status': 'down',
                'response_time_ms': elapsed,
                'details': f'Failed to reach {target}: {e}'
            }

    elif check_type == 'port':
        try:
            parts = target.rsplit(':', 1)
            host = parts[0]
            port_num = int(parts[1]) if len(parts) > 1 else 80
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(5)
            result = sock.connect_ex((host, port_num))
            sock.close()
            elapsed = int((time.time() - start) * 1000)
            if result == 0:
                return {
                    'status': 'up',
                    'response_time_ms': elapsed,
                    'details': f'Port {port_num} open on {host} ({elapsed}ms)'
                }
            else:
                return {
                    'status': 'down',
                    'response_time_ms': elapsed,
                    'details': f'Port {port_num} closed on {host}'
                }
        except Exception as e:
            elapsed = int((time.time() - start) * 1000)
            return {
                'status': 'down',
                'response_time_ms': elapsed,
                'details': f'Port check failed for {target}: {e}'
            }

    elif check_type == 'process':
        try:
            result = subprocess.run(['pgrep', '-f', target], capture_output=True, text=True, timeout=5)
            elapsed = int((time.time() - start) * 1000)
            if result.returncode == 0:
                pids = result.stdout.strip().split('\\n')
                return {
                    'status': 'up',
                    'response_time_ms': elapsed,
                    'details': f'Process \"{target}\" running ({len(pids)} instance(s), PIDs: {\",\".join(pids[:5])})'
                }
            else:
                return {
                    'status': 'down',
                    'response_time_ms': elapsed,
                    'details': f'Process \"{target}\" not found'
                }
        except Exception as e:
            elapsed = int((time.time() - start) * 1000)
            return {
                'status': 'down',
                'response_time_ms': elapsed,
                'details': f'Process check failed for {target}: {e}'
            }

    # Unknown check type - return mock
    return {
        'status': 'up',
        'response_time_ms': 42,
        'details': f'Mock check for {target} (type={check_type}): all OK'
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

def orchestrate_monitoring(payload):
    \"\"\"Run monitoring: check target, alert if issues found.\"\"\"
    start = time.time()
    target = payload.get('target', 'localhost')
    check_type = payload.get('check_type', 'http')
    alert_channel = payload.get('channel', 'log')

    # Step 1: Check the target
    check_result = watch_target(target, check_type)

    alert_result = None
    # Step 2: If target is not fully up, send an alert
    if check_result.get('status') != 'up':
        severity = 'critical' if check_result.get('status') == 'down' else 'warning'
        message = f'[{check_type.upper()}] {target} is {check_result[\"status\"]}: {check_result.get(\"details\", \"No details\")}'
        alert_result = invoke_remote('send-alert', 'alerter', {
            'message': message,
            'severity': severity,
            'channel': alert_channel,
        })

    duration = time.time() - start

    report = {
        'pipeline': 'monitor',
        'target': target,
        'check_type': check_type,
        'duration_seconds': round(duration, 2),
        'check': check_result,
    }
    if alert_result is not None:
        report['alert'] = alert_result.get('outputs', alert_result)

    print(json.dumps(report, indent=2), file=sys.stderr)
    return report

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[watcher] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        if length > 1048576:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(length)) if length > 0 else {}
        inputs = body.get('inputs', {})

        if self.path.startswith('/handle/watch-target'):
            result = watch_target(inputs.get('target', ''), inputs.get('check_type', 'http'))
            resp = json.dumps({'outputs': result})
            self.send_response(200)
        elif self.path.startswith('/handle/orchestrate'):
            result = orchestrate_monitoring(inputs)
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
print(f'[watcher] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
