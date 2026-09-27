#!/usr/bin/env python3
"""Resume one suspended GitHub App installation."""

from __future__ import annotations

import argparse
import base64
import json
import subprocess
import time
import urllib.request
from pathlib import Path


def encode_segment(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


def app_token(app_id: int, key_file: Path) -> str:
    now = int(time.time())
    header = encode_segment(b'{"alg":"RS256","typ":"JWT"}')
    payload = encode_segment(
        json.dumps({"iat": now - 60, "exp": now + 540, "iss": app_id}).encode("utf-8")
    )
    signed_text = f"{header}.{payload}"
    signature = subprocess.run(
        ["openssl", "dgst", "-sha256", "-sign", str(key_file)],
        input=signed_text.encode("ascii"),
        capture_output=True,
        check=True,
    ).stdout
    return f"{signed_text}.{encode_segment(signature)}"


def request_installation(installation_id: int, token: str, method: str) -> dict[str, str | int | None]:
    url = f"https://api.github.com/app/installations/{installation_id}"
    if method == "DELETE":
        url += "/suspended"
    request = urllib.request.Request(
        url,
        method=method,
        headers={
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {token}",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        if method == "DELETE":
            return {}
        result = json.load(response)
    if not isinstance(result, dict):
        raise ValueError("GitHub returned an invalid installation response")
    return result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--key-file", required=True, type=Path)
    parser.add_argument("--app-id", required=True, type=int)
    parser.add_argument("--installation-id", required=True, type=int)
    parser.add_argument("--status", action="store_true")
    args = parser.parse_args()

    token = app_token(args.app_id, args.key_file)
    installation = request_installation(args.installation_id, token, "GET")
    if installation.get("id") != args.installation_id:
        raise ValueError("GitHub returned a different installation")
    if args.status:
        state = "suspended" if installation.get("suspended_at") is not None else "active"
        print(f"GitHub App installation {args.installation_id} is {state}")
        return
    if installation.get("suspended_at") is not None:
        request_installation(args.installation_id, token, "DELETE")
        installation = request_installation(args.installation_id, token, "GET")
    if installation.get("suspended_at") is not None:
        raise RuntimeError("GitHub still reports the installation as suspended")
    print(f"GitHub App installation {args.installation_id} is active")


if __name__ == "__main__":
    main()
