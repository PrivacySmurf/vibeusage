#!/usr/bin/env python3
"""Redacting structure inspector for JSON and YAML credential files.

Prints key paths, container shapes, string lengths, and safe non-secret metadata
(timestamps, provider/source labels, booleans, numbers) without ever printing
secret string values (API keys, OAuth access/refresh tokens, cookies, or DSH
.credentials.yaml refs).

Usage:
    python3 scripts/inspect-creds.py <file> [dot.key.path]
"""

from __future__ import annotations

import json
import pathlib
import re
import sys
from typing import Any

SAFE_METADATA_KEYS = frozenset(
    {
        "browser",
        "created_at",
        "envKey",
        "expires_at",
        "field",
        "id",
        "item",
        "kind",
        "label",
        "plan",
        "provider",
        "source",
        "tier",
        "updated_at",
        "vault",
        "version",
        "vibeusage_owned",
    }
)

SECRET_KEY_SUBSTRINGS = (
    "key",
    "token",
    "secret",
    "password",
    "cookie",
    "auth",
    "credential",
    "bearer",
    "value",
)

SECRET_VALUE_PREFIXES = (
    "sk-",
    "eyJ",
    "ghp_",
    "gho_",
    "github_pat_",
    "VENICE_",
    "xox",
    "AIza",
)


def is_safe_metadata_string(key: str, path: str, val: str) -> bool:
    if not val:
        return True
    lower_key = key.lower()
    lower_path = path.lower()
    if any(part == "refs" for part in lower_path.split(".")):
        return False
    if any(s in lower_key for s in SECRET_KEY_SUBSTRINGS):
        return False
    if key not in SAFE_METADATA_KEYS:
        return False
    if len(val) > 64:
        return False
    if any(val.startswith(p) for p in SECRET_VALUE_PREFIXES):
        return False
    if re.search(r"[A-Za-z0-9_\-]{28,}", val):
        return False
    return True


def redact_tree(obj: Any, path: str = "", key: str = "") -> Any:
    if obj is None or isinstance(obj, (bool, int, float)):
        return obj
    if isinstance(obj, str):
        if is_safe_metadata_string(key, path, obj):
            return obj
        return f"<redacted str len={len(obj)}>"
    if isinstance(obj, list):
        return [
            redact_tree(item, f"{path}[{i}]", key) for i, item in enumerate(obj[:20])
        ] + ([f"... ({len(obj) - 20} more items)"] if len(obj) > 20 else [])
    if isinstance(obj, dict):
        out: dict[str, Any] = {}
        for k, v in obj.items():
            k_str = str(k)
            child_path = f"{path}.{k_str}" if path else k_str
            out[k_str] = redact_tree(v, child_path, k_str)
        return out
    return f"<{type(obj).__name__}>"


def load_file(path: pathlib.Path) -> Any:
    raw = path.read_text(encoding="utf-8")
    suffix = path.suffix.lower()
    if suffix in (".yaml", ".yml"):
        import yaml  # type: ignore[import-untyped]

        return yaml.safe_load(raw)
    return json.loads(raw)


def resolve_path(data: Any, dot_path: str) -> tuple[Any, str]:
    cur = data
    built = ""
    for part in dot_path.split("."):
        if not part:
            continue
        built = f"{built}.{part}" if built else part
        if isinstance(cur, dict) and part in cur:
            cur = cur[part]
        else:
            raise KeyError(f"path not found: {built}")
    return cur, built


def main(argv: list[str]) -> int:
    if len(argv) < 2 or len(argv) > 3 or argv[1] in ("-h", "--help"):
        print(__doc__.strip(), file=sys.stderr)
        return 0 if len(argv) >= 2 and argv[1] in ("-h", "--help") else 2

    file_path = pathlib.Path(argv[1]).expanduser()
    if not file_path.exists():
        print(f"error: file not found: {file_path}", file=sys.stderr)
        return 1

    data = load_file(file_path)
    dot_path = argv[2] if len(argv) == 3 else ""
    last_key = dot_path.split(".")[-1] if dot_path else ""
    if dot_path:
        data, resolved = resolve_path(data, dot_path)
    else:
        resolved = ""

    safe = redact_tree(data, path=resolved, key=last_key)
    print(json.dumps(safe, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
