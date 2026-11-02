#!/bin/bash
# App instance user-data: multi-purpose responder for LB E2E tests.
#   HTTP:80   serves JSON {"instance_id": "<hostname>"} for ALB suites,
#             and /__headers echoes the request headers
#   TCP:9000  echoes <hostname> for NLB TCP suites (also UDP-TG TCP health check)
#   UDP:9001  echoes <hostname> for NLB UDP suites
INSTANCE_ID=$(hostname)

mkdir -p /tmp/httpd/alpha /tmp/httpd/beta && cd /tmp/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > index.html
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > alpha/index.html
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > beta/index.html
echo ok > hc.html
cat > /tmp/httpd.py << 'PYEOF'
import functools, http.server, json

class Handler(http.server.SimpleHTTPRequestHandler):
    # /__headers echoes the request as the target sees it, one [name, value]
    # pair per header line so repeated headers stay visible.
    def do_GET(self):
        if self.path.split("?")[0] != "/__headers":
            return super().do_GET()
        body = json.dumps({"peer": self.client_address[0],
                           "headers": [list(p) for p in self.headers.items()]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a, **kw):
        pass

handler = functools.partial(Handler, directory="/tmp/httpd")
http.server.ThreadingHTTPServer(("0.0.0.0", 80), handler).serve_forever()
PYEOF
nohup python3 /tmp/httpd.py > /dev/null 2>&1 &

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

# TCP:8081 health switch the e2e driver flips: /health is 200 until /fail is
# requested, then 500 until /ok, and /fail also removes :80/hc.html (404) until
# /ok restores it. Data ports keep accepting.
cat > /tmp/health.py << 'PYEOF'
import http.server, os
FLAG = "/tmp/health-fail"
HC = "/tmp/httpd/hc.html"
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/fail":
            open(FLAG, "w").close()
            if os.path.exists(HC):
                os.remove(HC)
        elif self.path == "/ok":
            if os.path.exists(FLAG):
                os.remove(FLAG)
            open(HC, "w").write("ok\n")
        code = 500 if self.path == "/health" and os.path.exists(FLAG) else 200
        self.send_response(code); self.end_headers()
        self.wfile.write(b"ok\n")
    def log_message(self, *a, **kw):
        pass
http.server.ThreadingHTTPServer(("0.0.0.0", 8081), Handler).serve_forever()
PYEOF
nohup python3 /tmp/health.py > /dev/null 2>&1 &
