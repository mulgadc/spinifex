#!/bin/bash
# App instance user-data: dual-purpose responder for LB E2E tests.
#   HTTP:80   serves JSON {"instance_id": "<hostname>"} for ALB suites
#   TCP:9000  echoes <hostname> for NLB suites
INSTANCE_ID=$(hostname)

mkdir -p /tmp/httpd && cd /tmp/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > index.html
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
