# cpa-plugin — CLIProxyAPI Plugin Marketplace

A custom plugin store for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Host your own plugins and install them via the Management UI.

## Structure

```
.
├── registry.json          # Plugin store manifest (schema_version: 1)
├── README.md
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
  configs:
    opencode-free:
      enabled: true
      priority: 1
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

Releases are automated via GitHub Actions. Push a tag matching `{plugin-id}-{version}`:

```bash
git tag opencode-free-0.1.0
git push origin opencode-free-0.1.0
```

This triggers a build for 4 platforms (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64) and creates a GitHub release with the packaged artifacts.

When adding a new plugin to this repo, use the same pattern: `{plugin-directory-name}-{semver}`.
