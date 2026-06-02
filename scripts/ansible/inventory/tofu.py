#!/usr/bin/env python3
"""Dynamic ansible inventory generator from tofu-cluster tfvars.

Reads scripts/tofu-cluster/envs/<env>.tfvars (HCL subset) and emits ansible
JSON inventory. First node in the tfvars `nodes = [...]` list is the init
host (forms the cluster); the rest are join hosts.

Usage:
    inventory/tofu.py --list --env env1
    inventory/tofu.py --host <ip> --env env1

Selecting the env:
    CLUSTER_ENV=env1 inventory/tofu.py --list
    inventory/tofu.py --list --env env1
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path


def _repo_root() -> Path:
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "go.work").exists() or (parent / ".gitmodules").exists():
            return parent
    raise SystemExit(f"could not locate mulga repo root from {here}")


def _tfvars_path(env: str) -> Path:
    root = _repo_root()
    candidate = root / "scripts" / "tofu-cluster" / "envs" / f"{env}.tfvars"
    if not candidate.exists():
        raise SystemExit(f"tfvars file not found: {candidate}")
    return candidate


_STRING_RE = re.compile(r'^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*"([^"]*)"\s*$')
_NUMBER_RE = re.compile(r'^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*([0-9]+)\s*$')
_NODE_FIELD_RE = re.compile(r'(\w+)\s*=\s*"([^"]*)"')


def _parse_tfvars(path: Path) -> dict:
    """Minimal tfvars parser — handles the subset used by envs/*.tfvars.

    Supports:
      key = "string"
      key = 12345
      nodes = [
        { name = "x", wan_ip = "1.2.3.4", ... },
        ...
      ]
    """
    out: dict = {"nodes": []}
    text = path.read_text()

    # Strip line comments
    text = re.sub(r"#.*", "", text)

    # Pull the nodes block out first, then process remaining lines.
    nodes_match = re.search(r"nodes\s*=\s*\[(.+?)\]\s*$", text, re.DOTALL | re.MULTILINE)
    if nodes_match:
        nodes_body = nodes_match.group(1)
        for entry in re.finditer(r"\{([^}]+)\}", nodes_body):
            fields = dict(_NODE_FIELD_RE.findall(entry.group(1)))
            if "name" in fields and "wan_ip" in fields:
                out["nodes"].append(fields)
        text = text.replace(nodes_match.group(0), "")

    for line in text.splitlines():
        if m := _STRING_RE.match(line):
            out[m.group(1)] = m.group(2)
        elif m := _NUMBER_RE.match(line):
            out[m.group(1)] = int(m.group(2))

    return out


def _resolve_ssh_key(value: str | None) -> str:
    if not value:
        return os.path.expanduser("~/.ssh/tf-user-ap-southeast-2")
    return os.path.expanduser(value)


def _build_inventory(env: str) -> dict:
    data = _parse_tfvars(_tfvars_path(env))
    nodes = data.get("nodes") or []
    if not nodes:
        raise SystemExit(f"{env}.tfvars has no nodes")

    ssh_user = data.get("ssh_user", "tf-user")
    ssh_key = _resolve_ssh_key(data.get("ssh_public_key_path", "").replace(".pub", ""))

    hostvars: dict[str, dict] = {}
    all_hosts: list[str] = []
    init_hosts: list[str] = []
    join_hosts: list[str] = []

    for idx, node in enumerate(nodes):
        ip = node["wan_ip"]
        role = "init" if idx == 0 else "join"
        hostvars[ip] = {
            "ansible_host": ip,
            "ansible_user": ssh_user,
            "ansible_ssh_private_key_file": ssh_key,
            "ansible_ssh_common_args": "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR",
            "spinifex_node_name": node["name"],
            "spinifex_node_role": role,
            "spinifex_node_index": idx,
            "spinifex_wan_ip": ip,
        }
        all_hosts.append(ip)
        (init_hosts if role == "init" else join_hosts).append(ip)

    # Group-level vars derived from tfvars (operator can override with -e).
    cluster_init_host = init_hosts[0]
    group_vars = {
        "cluster_env": env,
        "cluster_init_host": cluster_init_host,
        "cluster_init_ip": cluster_init_host,
        "spinifex_node_count": len(nodes),
        "spinifex_region": data.get("region", "ap-southeast-2"),
        "spinifex_az": data.get("az", "ap-southeast-2a"),
    }
    if cidr := data.get("env_cidr"):
        group_vars["cluster_env_cidr"] = cidr

    inventory = {
        "_meta": {"hostvars": hostvars},
        env: {"hosts": all_hosts, "vars": group_vars},
        "cluster": {"children": [env]},
        "init": {"hosts": init_hosts},
        "join": {"hosts": join_hosts},
    }
    return inventory


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--list", action="store_true", help="emit full inventory")
    parser.add_argument("--host", help="emit hostvars for a single host (ignored — _meta carries them)")
    parser.add_argument(
        "--env",
        default=os.environ.get("CLUSTER_ENV"),
        help="tofu env name (matches scripts/tofu-cluster/envs/<env>.tfvars); also via CLUSTER_ENV env var",
    )
    args = parser.parse_args()

    if not args.env:
        sys.stderr.write("error: --env required (or CLUSTER_ENV)\n")
        return 64

    if args.host:
        json.dump({}, sys.stdout)
        sys.stdout.write("\n")
        return 0

    inventory = _build_inventory(args.env)
    json.dump(inventory, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
