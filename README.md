# cpa-plugin — CLIProxyAPI Plugin Marketplace

A custom plugin store for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Host your own plugins and install them via the Management UI.

## Structure

```
.
├── .github/
│   └── scripts/
│       ├── package-release.go  # zips plugin lib + sha256 for release
│       └── update-registry.go  # updates registry.json with artifact metadata
├── registry.json          # Plugin store manifest (schema_version: 2, direct install)
├── README.md
├── Makefile               # version bump / tag / release helpers
├── scripts/
│   └── bump_version.py    # updates registry.json version field
└── opencode-free/         # OpenCode Free plugin source
    ├── main.go            # Go plugin (buildmode=c-shared)
    ├── go.mod
```

## Plugins

### OpenCode Free

Free Claude models via [OpenCode](https://opencode.ai) — no authentication required.

**Models:** Claude Sonnet 4.7, Haiku 4.5, Opus 4.8, Fable 5

### Install from Marketplace

Add to CLIProxyAPI `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/nhymxu/cpa-plugin/main/registry.json"
```

Then install via Management UI or API:

```bash
curl -X POST -H "x-api-key: your-key" \
  http://localhost:8317/v0/management/plugin-store/opencode-free/install
```

### Manual Install

Download the zip for your platform from the [latest release](https://github.com/nhymxu/cpa-plugin/releases), unzip, and copy the library to your CLIProxyAPI plugins directory:

```bash
mkdir -p plugins/darwin/arm64
unzip opencode-free_0.1.0_darwin_arm64.zip -d plugins/darwin/arm64/
```

## Build

```bash
cd opencode-free
# macOS ARM64
go build -buildmode=c-shared -o opencode-free.dylib .
# Linux AMD64
GOOS=linux GOARCH=amd64 go build -buildmode=c-shared -o opencode-free.so .
```

## Release

Releases are automated via GitHub Actions, triggered by pushing a tag matching `{plugin-id}-{version}`.

Use the `Makefile` to bump `registry.json` and cut the tag:

```bash
make bump PLUGIN=opencode-free VERSION=0.1.5     # update registry.json only
make tag PLUGIN=opencode-free VERSION=0.1.5       # bump + create local git tag
make release PLUGIN=opencode-free VERSION=0.1.5   # bump + commit + tag + push (pushes to remote)
```

`PLUGIN` defaults to `opencode-free`, so it can be omitted for that plugin. Run `make help` for the full list.

Pushing the tag triggers a build for 4 platforms (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64), creates a GitHub release, and **automatically updates `registry.json`** with the new artifact URLs, SHA256 checksums, and sizes for direct install.

When adding a new plugin to this repo, update the `matrix.plugin-dir` list in `.github/workflows/release.yml` and add its entry to `registry.json` (the CI will fill in the `install.artifacts` section).

> **Note:** If the Management UI still fails with 502 on install, it's a [CLIProxyAPI bug](https://github.com/router-for-me/CLIProxyAPI) where the UI passes the full tag (e.g. `opencode-free-0.1.5`) as the version query param instead of bare semver (`0.1.5`). The workaround is to omit the `version` param entirely — the host auto-resolves the latest release.
