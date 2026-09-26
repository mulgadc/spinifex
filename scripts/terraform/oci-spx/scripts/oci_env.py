#!/usr/bin/env python3
"""Prepare OCI-profile Terraform environment variables in this repo's .venv.

Examples:
  eval "$(python3 scripts/oci_env.py --shell)"
  python3 scripts/oci_env.py -- terraform plan -out=mulgadc.tfplan
"""

from __future__ import annotations

import argparse
import configparser
import os
from pathlib import Path
import shlex
import subprocess
import sys
import venv


REPOSITORY = Path(__file__).resolve().parent.parent
VENV_PYTHON = REPOSITORY / ".venv" / "bin" / "python"
REQUIREMENTS = REPOSITORY / "requirements.txt"
REQUESTED_PROFILE = "apacanzset03child03"
FALLBACK_PROFILE = "apacanzset03child3"


def ensure_venv() -> None:
    """Create, populate, then re-execute from this repository's virtualenv."""
    if Path(sys.executable).resolve() == VENV_PYTHON.resolve():
        return
    if not VENV_PYTHON.exists():
        venv.EnvBuilder(with_pip=True).create(VENV_PYTHON.parent.parent)
    subprocess.check_call([str(VENV_PYTHON), "-m", "pip", "install", "--quiet", "-r", str(REQUIREMENTS)])
    os.execv(str(VENV_PYTHON), [str(VENV_PYTHON), str(Path(__file__).resolve()), *sys.argv[1:]])


def load_profile(config_path: Path, requested_profile: str) -> tuple[str, dict[str, str]]:
    parser = configparser.RawConfigParser()
    parser.read(config_path)
    profile = requested_profile
    if not parser.has_section(profile):
        if profile == REQUESTED_PROFILE and parser.has_section(FALLBACK_PROFILE):
            print(
                f"Profile {REQUESTED_PROFILE} not found; using {FALLBACK_PROFILE}.",
                file=sys.stderr,
            )
            profile = FALLBACK_PROFILE
        else:
            raise ValueError(f"OCI profile {profile!r} was not found in {config_path}")

    # Validate through the OCI SDK installed in the dedicated virtualenv.
    import oci

    values = oci.config.from_file(str(config_path), profile)
    required = ("tenancy", "user", "fingerprint", "key_file", "region")
    missing = [key for key in required if not values.get(key)]
    if missing:
        raise ValueError(f"Missing OCI profile values: {', '.join(missing)}")
    return profile, values


def tenancy_home_region(values: dict[str, str]) -> str:
    """Read the home region from OCI rather than assuming the profile region."""
    import oci

    subscriptions = oci.identity.IdentityClient(values).list_region_subscriptions(values["tenancy"]).data
    home = next((item.region_name for item in subscriptions if item.is_home_region), None)
    if not home:
        raise ValueError("OCI did not return a tenancy home region")
    return home


def terraform_environment(
    profile: str,
    values: dict[str, str],
    region_override: str | None,
    ssh_public_key_path: str | None,
    ssh_private_key_path: str | None,
) -> dict[str, str]:
    environment = {
        "OCI_CLI_PROFILE": profile,
        "TF_VAR_tenancy_ocid": values["tenancy"],
        "TF_VAR_user_ocid": values["user"],
        "TF_VAR_fingerprint": values["fingerprint"],
        "TF_VAR_private_key_path": values["key_file"],
        "TF_VAR_region": region_override or values["region"],
        "TF_VAR_home_region": tenancy_home_region(values),
    }
    if ssh_public_key_path:
        key_path = Path(ssh_public_key_path).expanduser().resolve()
        if not key_path.is_file():
            raise FileNotFoundError(f"SSH public key was not found at {key_path}")
        environment["TF_VAR_ssh_public_key_path"] = str(key_path)
    if ssh_private_key_path:
        key_path = Path(ssh_private_key_path).expanduser().resolve()
        if not key_path.is_file():
            raise FileNotFoundError(f"SSH private key was not found at {key_path}")
        environment["TF_VAR_ssh_private_key_path"] = str(key_path)
    return environment


def main() -> int:
    arg_parser = argparse.ArgumentParser()
    arg_parser.add_argument("--profile", default=REQUESTED_PROFILE)
    arg_parser.add_argument("--region")
    arg_parser.add_argument(
        "--ssh-public-key-path",
        help="Optional public-key path exported as TF_VAR_ssh_public_key_path.",
    )
    arg_parser.add_argument(
        "--ssh-private-key-path",
        help="Optional private-key path exported as TF_VAR_ssh_private_key_path.",
    )
    arg_parser.add_argument("--shell", action="store_true", help="Print shell exports for eval.")
    arg_parser.add_argument("command", nargs=argparse.REMAINDER, help="Command to run with the environment; prefix with --.")
    args = arg_parser.parse_args()
    if args.shell and args.command:
        arg_parser.error("--shell and a command cannot be used together")

    ensure_venv()
    config_path = Path(os.environ.get("OCI_CONFIG_FILE", Path.home() / ".oci" / "config"))
    if not config_path.is_file():
        raise FileNotFoundError(f"OCI configuration was not found at {config_path}")
    profile, values = load_profile(config_path, args.profile)
    environment = terraform_environment(
        profile,
        values,
        args.region,
        args.ssh_public_key_path,
        args.ssh_private_key_path,
    )

    if args.shell:
        for key, value in environment.items():
            print(f"export {key}={shlex.quote(value)}")
        return 0

    if not args.command:
        print(f"Loaded OCI profile {profile} for region {environment['TF_VAR_region']}; virtualenv: {VENV_PYTHON.parent.parent}")
        return 0

    command = args.command[1:] if args.command[0] == "--" else args.command
    if not command:
        raise ValueError("A command is required after --")
    child_environment = os.environ.copy()
    child_environment.update(environment)
    print(f"Running with OCI profile {profile} in {environment['TF_VAR_region']}: {' '.join(map(shlex.quote, command))}")
    return subprocess.call(command, cwd=REPOSITORY, env=child_environment)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (FileNotFoundError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Error: {error}", file=sys.stderr)
        raise SystemExit(1)
