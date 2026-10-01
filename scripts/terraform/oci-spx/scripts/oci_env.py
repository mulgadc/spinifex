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


ENVIRONMENT_KEYS = ("OCI_TENANCY_OCID", "OCI_USER_OCID", "OCI_FINGERPRINT", "OCI_PRIVATE_KEY", "OCI_REGION")


def load_environment() -> dict[str, str] | None:
    """Read the credential from the environment, the way a CI runner holds it.

    Returns None when none of the keys are set, so a workstation still falls
    through to ~/.oci/config. A partial set is an error rather than a fallback:
    half a credential in the environment means a secret failed to reach the job,
    and silently using a different one would validate the wrong tenancy.
    """
    present = [key for key in ENVIRONMENT_KEYS if os.environ.get(key)]
    if not present:
        return None
    missing = [key for key in ENVIRONMENT_KEYS if not os.environ.get(key)]
    if missing:
        raise ValueError(
            f"OCI credential in the environment is incomplete: {', '.join(present)} set, {', '.join(missing)} missing"
        )
    key = os.environ["OCI_PRIVATE_KEY"]
    # Base64 is accepted because a PEM's newlines do not survive every secret
    # store. Detected by shape rather than by a second variable to set wrongly.
    if "-----BEGIN" not in key:
        import base64

        key = base64.b64decode(key).decode()
    if "-----BEGIN" not in key:
        raise ValueError("OCI_PRIVATE_KEY is neither a PEM nor base64-encoded PEM")
    return {
        "tenancy": os.environ["OCI_TENANCY_OCID"],
        "user": os.environ["OCI_USER_OCID"],
        "fingerprint": os.environ["OCI_FINGERPRINT"],
        "key_content": key,
        "region": os.environ["OCI_REGION"],
    }


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
        "TF_VAR_tenancy_ocid": values["tenancy"],
        "TF_VAR_user_ocid": values["user"],
        "TF_VAR_fingerprint": values["fingerprint"],
        "TF_VAR_region": region_override or values["region"],
        "TF_VAR_home_region": tenancy_home_region(values),
    }
    # One of the two, never both: the provider rejects a key given twice, and the
    # variable validation in variables.tf says so before OCI is called at all.
    if values.get("key_content"):
        environment["TF_VAR_private_key"] = values["key_content"]
    else:
        environment["OCI_CLI_PROFILE"] = profile
        environment["TF_VAR_private_key_path"] = values["key_file"]
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
    values = load_environment()
    if values is not None:
        profile = "environment"
        # Checked here, before anything calls OCI: --shell prints to stdout, which
        # in CI is the job log, and this route holds the PEM itself.
        if args.shell:
            raise ValueError("--shell cannot be used with a credential from the environment: it would print the key")
    else:
        config_path = Path(os.environ.get("OCI_CONFIG_FILE", Path.home() / ".oci" / "config"))
        if not config_path.is_file():
            raise FileNotFoundError(
                f"OCI configuration was not found at {config_path} and the environment holds no credential"
                f" (set {', '.join(ENVIRONMENT_KEYS)})"
            )
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
    # Stderr, because stdout belongs to the wrapped command: a banner there is
    # indistinguishable from output and corrupts anything that captures it.
    print(
        f"Running with OCI profile {profile} in {environment['TF_VAR_region']}: {' '.join(map(shlex.quote, command))}",
        file=sys.stderr,
    )
    return subprocess.call(command, cwd=REPOSITORY, env=child_environment)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (FileNotFoundError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Error: {error}", file=sys.stderr)
        raise SystemExit(1)
