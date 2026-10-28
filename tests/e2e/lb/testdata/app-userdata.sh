#!/bin/bash
# App instance user-data: multi-purpose responder for LB E2E tests.
#   HTTP:80   serves JSON {"instance_id": "<hostname>"} for ALB suites
#   TCP:9000  echoes <hostname> for NLB TCP suites (also UDP-TG TCP health check)
#   UDP:9001  echoes <hostname> for NLB UDP suites
INSTANCE_ID=$(hostname)

mkdir -p /tmp/httpd/alpha /tmp/httpd/beta && cd /tmp/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > index.html
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > alpha/index.html
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > beta/index.html
nohup python3 -m http.server 80 --bind 0.0.0.0 > /dev/null 2>&1 &

cat > /tmp/tcp_echo.py << 'PYEOF'
import socketserver, os
class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.wfile.write((os.uname()[1] + "\n").encode())
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("0.0.0.0", 9000), Handler).serve_forever()
PYEOF
nohup python3 /tmp/tcp_echo.py > /dev/null 2>&1 &

cat > /tmp/udp_echo.py << 'PYEOF'
import socketserver, os
class Handler(socketserver.DatagramRequestHandler):
    def handle(self):
        self.wfile.write((os.uname()[1] + "\n").encode())
socketserver.UDPServer.allow_reuse_address = True
socketserver.UDPServer(("0.0.0.0", 9001), Handler).serve_forever()
PYEOF
nohup python3 /tmp/udp_echo.py > /dev/null 2>&1 &

# TCP:8081 health endpoint whose answer the e2e driver flips: /health is 200
# until /fail is requested, then 500 until /ok. Data ports keep accepting.
cat > /tmp/health.py << 'PYEOF'
import http.server, os
FLAG = "/tmp/health-fail"
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/fail":
            open(FLAG, "w").close()
        elif self.path == "/ok" and os.path.exists(FLAG):
            os.remove(FLAG)
        code = 500 if self.path == "/health" and os.path.exists(FLAG) else 200
        self.send_response(code); self.end_headers()
        self.wfile.write(b"ok\n")
    def log_message(self, *a, **kw):
        pass
http.server.ThreadingHTTPServer(("0.0.0.0", 8081), Handler).serve_forever()
PYEOF
nohup python3 /tmp/health.py > /dev/null 2>&1 &
