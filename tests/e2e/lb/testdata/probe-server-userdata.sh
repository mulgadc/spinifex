#!/bin/bash
# Client VM user-data: starts a long-lived probe server on port 80.
# The test driver POSTs (well, GETs) /probe?ip=...&proto=http|tcp&n=20
# from outside the VPC; the server runs curl/nc inside the VPC to the
# requested LB private IP and returns the raw responses, one per line.
mkdir -p /tmp/probe && cd /tmp/probe

cat > server.py << 'PYEOF'
import http.server, subprocess, urllib.parse

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args, **kwargs):
        pass

    def do_GET(self):
        if self.path == '/status':
            self.send_response(200)
            self.send_header('Content-Type', 'text/plain')
            self.end_headers()
            self.wfile.write(b'ready')
            return
        if not self.path.startswith('/probe'):
            self.send_response(404)
            self.end_headers()
            return
        params = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(self.path).query))
        ip = params.get('ip', '')
        proto = params.get('proto', 'http')
        try:
            n = int(params.get('n', '20'))
        except ValueError:
            n = 20
        lines = []
        for _ in range(n):
            try:
                if proto == 'http':
                    out = subprocess.check_output(
                        ['curl', '-s', '--max-time', '5', f'http://{ip}:80/'],
                        timeout=10, stderr=subprocess.DEVNULL,
                    )
                else:
                    out = subprocess.check_output(
                        f"echo '' | nc -w2 {ip} 9000",
                        shell=True, timeout=10, stderr=subprocess.DEVNULL,
                    )
                lines.append(out.decode(errors='replace').strip())
            except Exception:
                lines.append('')
        body = '\n'.join(lines).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

http.server.HTTPServer.allow_reuse_address = True
http.server.HTTPServer(('0.0.0.0', 80), Handler).serve_forever()
PYEOF

nohup python3 server.py > /tmp/probe/server.log 2>&1 &
