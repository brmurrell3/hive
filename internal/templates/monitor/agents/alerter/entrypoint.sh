#!/usr/bin/env bash
# Hive Monitor - Alerter Agent
# Sends alert notifications via various channels.
# Uses ANTHROPIC_API_KEY if available for enriched alert messages, otherwise sends as-is.
set -euo pipefail

PORT="${HIVE_CALLBACK_PORT:-9201}"

python3 -c "
import http.server, json, urllib.request, os, sys, signal, datetime

PORT = int(os.environ.get('HIVE_CALLBACK_PORT', '9201'))
API_KEY = os.environ.get('ANTHROPIC_API_KEY', '')

def send_alert(message, severity='info', channel='log'):
    \"\"\"Send an alert notification.\"\"\"
    timestamp = datetime.datetime.utcnow().strftime('%Y-%m-%dT%H:%M:%SZ')

    # Optionally enrich the message with AI analysis
    enriched = message
    if API_KEY and severity in ('warning', 'critical'):
        try:
            prompt = f'Analyze this monitoring alert and suggest remediation steps in 2-3 sentences. Alert ({severity}): {message}'
            data = json.dumps({
                'model': 'claude-sonnet-4-5-20250514',
                'max_tokens': 256,
                'messages': [{'role': 'user', 'content': prompt}]
            }).encode()
            req = urllib.request.Request('https://api.anthropic.com/v1/messages',
                data=data,
                headers={'Content-Type': 'application/json', 'x-api-key': API_KEY, 'anthropic-version': '2023-06-01'})
            with urllib.request.urlopen(req, timeout=15) as resp:
                result = json.loads(resp.read())
                analysis = result['content'][0]['text']
                enriched = f'{message}\n\nAI Analysis: {analysis}'
        except Exception as e:
            print(f'[alerter] AI enrichment failed: {e}', file=sys.stderr)

    severity_prefix = {'info': 'INFO', 'warning': 'WARN', 'critical': 'CRIT'}
    prefix = severity_prefix.get(severity, 'INFO')

    if channel == 'log':
        print(f'[alerter] [{prefix}] {timestamp} {enriched}', file=sys.stderr)
        delivered = True
    elif channel == 'webhook':
        webhook_url = os.environ.get('HIVE_WEBHOOK_URL', '')
        if webhook_url:
            try:
                payload = json.dumps({
                    'text': enriched,
                    'severity': severity,
                    'timestamp': timestamp
                }).encode()
                req = urllib.request.Request(webhook_url, data=payload,
                    headers={'Content-Type': 'application/json'})
                with urllib.request.urlopen(req, timeout=10) as resp:
                    delivered = resp.getcode() < 400
            except Exception as e:
                print(f'[alerter] Webhook delivery failed: {e}', file=sys.stderr)
                delivered = False
        else:
            print(f'[alerter] [{prefix}] {timestamp} (webhook not configured, logging instead) {enriched}', file=sys.stderr)
            delivered = True
    else:
        print(f'[alerter] [{prefix}] {timestamp} {enriched}', file=sys.stderr)
        delivered = True

    return {
        'delivered': delivered,
        'channel': channel,
        'timestamp': timestamp
    }

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f'[alerter] {fmt % args}', file=sys.stderr)

    def do_POST(self):
        if self.path.startswith('/handle/send-alert'):
            length = int(self.headers.get('Content-Length', 0))
            if length > 1048576:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(length)) if length > 0 else {}
            inputs = body.get('inputs', {})
            result = send_alert(
                inputs.get('message', ''),
                inputs.get('severity', 'info'),
                inputs.get('channel', 'log')
            )
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
print(f'[alerter] Listening on 127.0.0.1:{PORT}', file=sys.stderr)
server.serve_forever()
"
