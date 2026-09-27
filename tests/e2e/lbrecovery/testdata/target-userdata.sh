#!/bin/bash
# Backend for the load balancer recovery suite: one HTTP responder on :80 that
# names itself, so a round of probes shows which backend answered and whether
# both are still being reached.
INSTANCE_ID=$(hostname)

mkdir -p /tmp/httpd && cd /tmp/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > index.html
nohup python3 -m http.server 80 --bind 0.0.0.0 > /dev/null 2>&1 &
