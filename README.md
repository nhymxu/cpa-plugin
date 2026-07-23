# cpa-plugin — CLIProxyAPI Plugin Marketplace

A custom plugin store for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Host your own plugins and install them via the Management UI.

## Structure

```
.
├── registry.json          # Plugin store manifest (schema_version: 2)
├── README.md
└── opencode-free/         # OpenCode Free plugin source
    ├── main.go            # Go plugin (buildmode=c-shared)
    ├── go.mod
    └── opencode-free-plugin.dylib  # Prebuilt macOS ARM64
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
    opencode-free-plugin:
      enabled: true
      priority: 1
```

Then install via Management UI or API:

```bash
curl -X POST -H "x-api-key: your-key" \
  http://localhost:8317/v0/management/plugin-store/opencode-free-plugin/install
```

### Manual Install

```bash
# macOS ARM
mkdir -p plugins/darwin/arm64
cp opencode-free/opencode-free-plugin.dylib plugins/darwin/arm64/
```

## Build

```bash
cd opencode-free
go build -buildmode=c-shared -o opencode-free-plugin.dylib .
```
