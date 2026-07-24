#!/usr/bin/env python3
"""Bump a plugin's version in registry.json and reset install artifacts for the new version."""
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
    idx = next((i for i, p in enumerate(data["plugins"]) if p["id"] == plugin_id), None)
    if idx is None:
        sys.exit(f"plugin '{plugin_id}' not found in {REGISTRY_PATH}")

    # Find the plugin block start
    id_match = re.search(r'"id"\s*:\s*"' + re.escape(plugin_id) + r'"', text)
    if not id_match:
        sys.exit(f"plugin id '{plugin_id}' not found in text")

    # Update version
    version_match = re.search(r'("version"\s*:\s*")([^"]*)(")', text[id_match.end():])
    if not version_match:
        sys.exit(f"no version field found for plugin '{plugin_id}'")
    start = id_match.end() + version_match.start(2)
    end = id_match.end() + version_match.end(2)
    old_version = text[start:end]

    # Replace version in text
    text = text[:start] + version + text[end:]

    # If there's an existing install.artifacts block, clear it so CI fills it in
    install_match = re.search(
        r'("install"\s*:\s*\{\s*"type"\s*:\s*"direct"\s*,\s*"artifacts"\s*:\s*)\[[^\]]*\](\s*\})',
        text,
    )
    if install_match:
        text = text[: install_match.start(1)] + install_match.group(1) + "[]" + install_match.group(2)

    with open(REGISTRY_PATH, "w") as f:
        f.write(text)

    print(f"{plugin_id}: {old_version} -> {version}")


if __name__ == "__main__":
    main()