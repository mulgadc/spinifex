#!/bin/bash
# Shared probe client. Long-lived across both internal LB suites — saves
# the ~60s second cold boot the per-suite client would otherwise incur.
#
# - Port 80: python http.server, serves /tmp/httpd (status + results files)
# - Port 9090: tiny trigger server. POST JSON {proto, ip, count, outfile}
#              runs the probe loop synchronously and writes the result
#              file under /tmp/httpd so the driver can curl it back.

set -eu
mkdir -p /tmp/httpd
cd /tmp/httpd
echo "ready" > status.txt
nohup python3 -m http.server 80 --bind 0.0.0.0 > /dev/null 2>&1 &

cat > /tmp/trigger.py <<'PYEOF'
import http.server, json, os, socketserver, subprocess

OUT_DIR = "/tmp/httpd"

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n).decode())
            proto = body["proto"]
            ip = body["ip"]
            count = int(body.get("count", 20))
            outfile = body["outfile"]
            tcp_port = int(body.get("tcp_port", 9000))
            udp_port = int(body.get("udp_port", 9001))
            http_port = int(body.get("http_port", 80))
            http_path = body.get("http_path", "/") or "/"
            if not http_path.startswith("/"):
                http_path = "/" + http_path
            host_header = body.get("host", "")
        except Exception as exc:
            self.send_response(400); self.end_headers()
            self.wfile.write(f"bad request: {exc}".encode())
            return

        path = os.path.join(OUT_DIR, outfile)
        with open(path, "w") as fh:
            for _ in range(count):
                if proto == "http":
                    cmd = ["curl", "-s", "--max-time", "5"]
                    if host_header:
                        cmd += ["-H", f"Host: {host_header}"]
                    cmd.append(f"http://{ip}:{http_port}{http_path}")
                    r = subprocess.run(cmd, capture_output=True, text=True)
                    fh.write(r.stdout.strip() + "\n")
                elif proto == "udp":
                    r = subprocess.run(
                        ["bash", "-c", f'echo "ping" | nc -u -w2 {ip} {udp_port} 2>/dev/null || true'],
                        capture_output=True, text=True)
                    fh.write(r.stdout.strip() + "\n")
                else:
                    r = subprocess.run(
                        ["bash", "-c", f'echo "" | nc -w2 {ip} {tcp_port} 2>/dev/null || true'],
                        capture_output=True, text=True)
                    fh.write(r.stdout.strip() + "\n")
        self.send_response(200); self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *a, **kw):
        pass

socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("0.0.0.0", 9090), Handler).serve_forever()
PYEOF
nohup python3 /tmp/trigger.py > /tmp/trigger.log 2>&1 &
