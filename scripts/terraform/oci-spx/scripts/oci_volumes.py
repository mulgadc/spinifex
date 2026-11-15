#!/usr/bin/env python3
"""Find and sweep the OCI volumes and backups ocibsd tags, which no Terraform state holds.

Run under oci_env.py for the credential, with the compartment in OCIBSD_COMPARTMENT.
Standard library and the openssl binary only; the key reaches openssl through a pipe.

  oci_volumes.py volumes --tag spinifex-volume-id=vol-0abc
  oci_volumes.py backups --tag spinifex-snapshot-id=snap-0abc
  oci_volumes.py sweep --cluster <cluster_id> [--timeout 900]
"""

from __future__ import annotations

import argparse
import base64
import email.utils
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

API = "/20160918"
# Gone for good; OCI keeps the record for a while and it cannot be deleted again.
GONE = {"TERMINATED"}
# OCI's own answers to a delete it will accept later: an attachment or a backup
# still in progress (409), or a precondition such as a busy lifecycle (412).
RETRY_LATER = {409, 412, 429, 500, 502, 503, 504}


class OCIError(Exception):
    def __init__(self, status: int, body: str):
        super().__init__(f"OCI answered {status}: {body[:300]}")
        self.status = status


class Client:
    def __init__(self, env: dict[str, str]):
        missing = [k for k in ("TF_VAR_tenancy_ocid", "TF_VAR_user_ocid", "TF_VAR_fingerprint", "TF_VAR_region") if not env.get(k)]
        if missing:
            raise ValueError(f"run this under oci_env.py: {', '.join(missing)} unset")
        self.key_id = f"{env['TF_VAR_tenancy_ocid']}/{env['TF_VAR_user_ocid']}/{env['TF_VAR_fingerprint']}"
        self.host = f"iaas.{env['TF_VAR_region']}.oraclecloud.com"
        if env.get("TF_VAR_private_key"):
            self.key = env["TF_VAR_private_key"].encode()
        elif env.get("TF_VAR_private_key_path"):
            self.key = Path(env["TF_VAR_private_key_path"]).read_bytes()
        else:
            raise ValueError("run this under oci_env.py: no private key in TF_VAR_private_key or TF_VAR_private_key_path")

    def _sign(self, data: bytes) -> str:
        read_end, write_end = os.pipe()
        try:
            os.write(write_end, self.key)
        finally:
            os.close(write_end)
        try:
            result = subprocess.run(
                ["openssl", "dgst", "-sha256", "-sign", f"/dev/fd/{read_end}"],
                input=data,
                capture_output=True,
                pass_fds=(read_end,),
                check=False,
            )
        finally:
            os.close(read_end)
        if result.returncode != 0:
            raise ValueError(f"openssl could not sign with the OCI key: {result.stderr.decode(errors='replace').strip()}")
        return base64.b64encode(result.stdout).decode()

    def request(self, method: str, path: str, query: dict[str, str] | None = None) -> tuple[dict[str, str], object]:
        target = API + path
        if query:
            target += "?" + urllib.parse.urlencode(query)
        date = email.utils.formatdate(usegmt=True)
        headers = {"date": date, "host": self.host}
        signed = ["(request-target)", "date", "host"]
        lines = [f"(request-target): {method.lower()} {target}", f"date: {date}", f"host: {self.host}"]
        # A PUT or POST also signs its body, which this signer does not do.
        if method not in ("GET", "DELETE"):
            raise ValueError(f"unsupported method {method}")
        signature = self._sign("\n".join(lines).encode())
        headers["authorization"] = (
            f'Signature version="1",keyId="{self.key_id}",algorithm="rsa-sha256",'
            f'headers="{" ".join(signed)}",signature="{signature}"'
        )
        req = urllib.request.Request(f"https://{self.host}{target}", method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                body = resp.read()
                return dict(resp.headers), json.loads(body) if body else None
        except urllib.error.HTTPError as error:
            raise OCIError(error.code, error.read().decode(errors="replace")) from None

    def list_all(self, kind: str, compartment: str) -> list[dict]:
        items: list[dict] = []
        query = {"compartmentId": compartment, "limit": "100"}
        while True:
            headers, page = self.request("GET", f"/{kind}", query)
            items.extend(page or [])
            next_page = {k.lower(): v for k, v in headers.items()}.get("opc-next-page")
            if not next_page:
                return items
            query["page"] = next_page


def matching(items: list[dict], tags: dict[str, str]) -> list[dict]:
    """The live items carrying every tag, with no OCID beyond the item's own."""
    found = []
    for item in items:
        freeform = item.get("freeformTags") or {}
        if item.get("lifecycleState") in GONE:
            continue
        if all(freeform.get(k) == v for k, v in tags.items()):
            found.append(
                {
                    "id": item["id"],
                    "displayName": item.get("displayName"),
                    "lifecycleState": item.get("lifecycleState"),
                    "freeformTags": freeform,
                }
            )
    return found


def parse_tags(pairs: list[str]) -> dict[str, str]:
    tags = {}
    for pair in pairs:
        key, sep, value = pair.partition("=")
        if not sep or not key:
            raise ValueError(f"--tag wants key=value, got {pair!r}")
        tags[key] = value
    return tags


def sweep(client: Client, compartment: str, cluster: str, timeout: int, interval: int) -> int:
    """Delete every live volume and backup tagged with this cluster until none is left.

    OCI refuses both deletes while a backup is CREATING, so a refusal is retried
    until the deadline, which is what makes a run killed mid-snapshot sweepable.
    """
    tags = {"spinifex-cluster": cluster}
    deadline = time.monotonic() + timeout
    while True:
        left = []
        for kind in ("volumeBackups", "volumes"):
            for item in matching(client.list_all(kind, compartment), tags):
                left.append((kind, item))
                if item["lifecycleState"] == "TERMINATING":
                    continue
                try:
                    client.request("DELETE", f"/{kind}/{item['id']}")
                    print(f"sweep: deleting {kind[:-1]} {item['displayName']} ({item['lifecycleState']})")
                except OCIError as error:
                    if error.status == 404:
                        continue
                    if error.status not in RETRY_LATER:
                        raise
                    print(f"sweep: {kind[:-1]} {item['displayName']} not deletable yet: {error.status}")
        if not left:
            print(f"sweep: nothing tagged spinifex-cluster={cluster} is left")
            return 0
        if time.monotonic() >= deadline:
            for kind, item in left:
                print(f"sweep: STILL PRESENT {kind[:-1]} {item['displayName']} {item['lifecycleState']}")
            print(f"sweep: {len(left)} tagged resource(s) left after {timeout}s")
            return 1
        time.sleep(interval)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("volumes", "backups"):
        listing = sub.add_parser(name, help=f"print the live {name} carrying every --tag, as JSON")
        listing.add_argument("--tag", action="append", default=[], required=True)
    sweeping = sub.add_parser("sweep", help="delete every live volume and backup tagged with --cluster")
    sweeping.add_argument("--cluster", required=True)
    sweeping.add_argument("--timeout", type=int, default=900)
    sweeping.add_argument("--interval", type=int, default=15)
    args = parser.parse_args()

    compartment = os.environ.get("OCIBSD_COMPARTMENT", "")
    if not compartment.startswith("ocid1."):
        raise ValueError("OCIBSD_COMPARTMENT is not set to a compartment OCID")
    client = Client(dict(os.environ))

    if args.command == "sweep":
        if not args.cluster.strip():
            raise ValueError("--cluster is empty, which would match nothing and report clean")
        return sweep(client, compartment, args.cluster, args.timeout, args.interval)
    kind = "volumes" if args.command == "volumes" else "volumeBackups"
    print(json.dumps(matching(client.list_all(kind, compartment), parse_tags(args.tag))))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OCIError, ValueError, OSError) as error:
        print(f"oci_volumes.py: {error}", file=sys.stderr)
        raise SystemExit(1)
