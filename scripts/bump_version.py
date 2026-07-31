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
        data = json.load(f)

    plugin = next((p for p in data["plugins"] if p["id"] == plugin_id), None)
    if plugin is None:
        sys.exit(f"plugin '{plugin_id}' not found in {REGISTRY_PATH}")

    old_version = plugin["version"]
    plugin["version"] = version

    # Reset this plugin's own install.artifacts so CI fills it in for the new version.
    if "install" in plugin and "artifacts" in plugin["install"]:
        plugin["install"]["artifacts"] = []

    with open(REGISTRY_PATH, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")

    print(f"{plugin_id}: {old_version} -> {version}")


if __name__ == "__main__":
    main()