#!/usr/bin/env python3
"""Bump a plugin's version field in registry.json without reformatting the file."""
import json
import re
import sys

REGISTRY_PATH = "registry.json"


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: bump_version.py <plugin-id> <version>")

    plugin_id, version = sys.argv[1], sys.argv[2]
    if not re.fullmatch(r"\d+\.\d+\.\d+", version):
        sys.exit(f"invalid version '{version}', expected semver x.y.z")

    with open(REGISTRY_PATH) as f:
        text = f.read()

    data = json.loads(text)
    if not any(p["id"] == plugin_id for p in data["plugins"]):
        sys.exit(f"plugin '{plugin_id}' not found in {REGISTRY_PATH}")

    id_match = re.search(r'"id"\s*:\s*"' + re.escape(plugin_id) + r'"', text)
    version_match = re.search(r'("version"\s*:\s*")([^"]*)(")', text[id_match.end():])
    if not version_match:
        sys.exit(f"no version field found for plugin '{plugin_id}'")

    start = id_match.end() + version_match.start(2)
    end = id_match.end() + version_match.end(2)
    old_version = text[start:end]
    new_text = text[:start] + version + text[end:]

    with open(REGISTRY_PATH, "w") as f:
        f.write(new_text)

    print(f"{plugin_id}: {old_version} -> {version}")


if __name__ == "__main__":
    main()
