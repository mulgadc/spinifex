#!/usr/bin/env bash
# Run `aws` CLI inside a shell that has the `spinifex` supplementary group
# active, with AWS_PROFILE=spinifex preset. Works around mulga-siv-92:
# ansible's `become` is a no-op when the target user equals the calling
# user, so `sudo -u` is never invoked and supplementary groups are not
# re-initialised. Without the `spinifex` group, /etc/spinifex/ca.pem (the
# AWS endpoint CA bundle) is unreadable and aws fails with SSL Errno 13.
#
# Usage from ansible tasks:
#   ansible.builtin.command:
#     cmd: "{{ playbook_dir }}/../scripts/aws-as-spinifex.sh ec2 describe-regions"
#
# All args are forwarded to `aws`. Quoting is preserved via printf %q so
# things like `--query 'length(Images)'` reach aws intact.
set -euo pipefail

if [[ $# -eq 0 ]]; then
    echo "usage: $0 <aws-cli-args>" >&2
    exit 64
fi

cmd="AWS_PROFILE=spinifex aws"
for arg in "$@"; do
    cmd+=" $(printf '%q' "$arg")"
done

exec sg spinifex -c "$cmd"
