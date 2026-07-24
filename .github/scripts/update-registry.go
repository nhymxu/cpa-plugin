package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type artifact struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type plugin struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Author      string     `json:"author"`
	Version     string     `json:"version"`
	Repository  string     `json:"repository,omitempty"`
	Homepage    string     `json:"homepage,omitempty"`
	License     string     `json:"license,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	Install     installDef `json:"install"`
}

type installDef struct {
	Type      string     `json:"type"`
	Artifacts []artifact `json:"artifacts"`
}

type registry struct {
	SchemaVersion int      `json:"schema_version"`
	Plugins       []plugin `json:"plugins"`
}

var assetPatterns = map[string]struct {
	goos  string
	goarch string
}{
	"darwin_arm64": {goos: "darwin", goarch: "arm64"},
	"darwin_amd64": {goos: "darwin", goarch: "amd64"},
	"linux_amd64":  {goos: "linux", goarch: "amd64"},
	"linux_arm64":  {goos: "linux", goarch: "arm64"},
}

func main() {
	registryPath := flag.String("registry", "registry.json", "path to registry.json")
	pluginID := flag.String("plugin", "", "plugin ID (e.g. opencode-free)")
	version := flag.String("version", "", "plugin version (e.g. 0.1.5)")

	artifactFlags := flag.String("artifacts", "", `JSON array of artifacts: [{"goos":"darwin","goarch":"arm64","url":"...","sha256":"...","size":123}]`)
	scanDir := flag.String("scan", "", "directory with downloaded release assets to auto-discover (zip + sha256 files)")
	flag.Parse()

	if *pluginID == "" {
		fatalf("plugin is required")
	}
	if *version == "" {
		fatalf("version is required")
	}

	reg := readRegistry(*registryPath)

	var artifacts []artifact
	if *artifactFlags != "" {
		if err := json.Unmarshal([]byte(*artifactFlags), &artifacts); err != nil {
			fatalf("parse artifacts: %v", err)
		}
	} else if *scanDir != "" {
		artifacts = scanAssets(*scanDir, *pluginID, *version)
	}

	if len(artifacts) == 0 {
		fatalf("no artifacts provided — use -artifacts or -scan")
	}

	// Find or create the plugin entry
	idx := -1
	for i, p := range reg.Plugins {
		if p.ID == *pluginID {
			idx = i
			break
		}
	}
	if idx == -1 {
		fatalf("plugin %q not found in registry", *pluginID)
	}

	reg.Plugins[idx].Version = *version
	reg.Plugins[idx].Install = installDef{
		Type:      "direct",
		Artifacts: artifacts,
	}

	writeRegistry(*registryPath, reg)
	fmt.Printf("Updated registry: %s — plugin %s v%s with %d artifacts\n",
		*registryPath, *pluginID, *version, len(artifacts))
}

func readRegistry(path string) registry {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read registry: %v", err)
	}

	// First pass: unmarshal bare (schema 1 or 2)
	var raw struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		fatalf("decode registry schema: %v", err)
	}

	if raw.SchemaVersion == 1 {
		// schema 1 has no install fields — convert to schema 2
		var v1 struct {
			SchemaVersion int              `json:"schema_version"`
			Plugins       []json.RawMessage `json:"plugins"`
		}
		if err := json.Unmarshal(data, &v1); err != nil {
			fatalf("decode registry v1: %v", err)
		}
		reg := registry{SchemaVersion: 2}
		for _, rawPlugin := range v1.Plugins {
			var p plugin
			if err := json.Unmarshal(rawPlugin, &p); err != nil {
				fatalf("decode plugin: %v", err)
			}
			// strip repository for direct install
			reg.Plugins = append(reg.Plugins, p)
		}
		return reg
	}

	var reg registry
	if err := json.Unmarshal(data, &reg); err != nil {
		fatalf("decode registry v2: %v", err)
	}
	reg.SchemaVersion = 2
	return reg
}

func writeRegistry(path string, reg registry) {
	reg.SchemaVersion = 2
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		fatalf("encode registry: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fatalf("write registry: %v", err)
	}
}

func scanAssets(dir, pluginID, version string) []artifact {
	dir = strings.TrimRight(dir, "/")
	entries, err := os.ReadDir(dir)
	if err != nil {
		fatalf("scan dir %s: %v", dir, err)
	}

	// Collect zip files and their sha256 companions
	zipMap := make(map[string]string) // platform key -> zip path
	shaMap := make(map[string]string) // platform key -> sha256 hex

	prefix := pluginID + "_" + version + "_"

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix) // e.g. "darwin_arm64.zip" or "darwin_arm64.zip.sha256"
		if strings.HasSuffix(rest, ".sha256") {
			platformKey := strings.TrimSuffix(rest, ".sha256") // e.g. "darwin_arm64.zip"
			platformKey = strings.TrimSuffix(platformKey, ".zip")
			shaMap[platformKey] = filepath.Join(dir, entry.Name())
		} else if strings.HasSuffix(rest, ".zip") {
			platformKey := strings.TrimSuffix(rest, ".zip")
			zipMap[platformKey] = filepath.Join(dir, entry.Name())
		}
	}

	if len(zipMap) == 0 {
		fatalf("no zip files found in %s matching %s_%s_*", dir, pluginID, version)
	}

	tag := pluginID + "-" + version
	repo := "https://github.com/nhymxu/cpa-plugin"

	artifacts := make([]artifact, 0)
	for platformKey := range zipMap {
		pattern, ok := assetPatterns[platformKey]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: unknown platform %q, skipping\n", platformKey)
			continue
		}

		zipPath := zipMap[platformKey]
		info, err := os.Stat(zipPath)
		if err != nil {
			fatalf("stat %s: %v", zipPath, err)
		}

		// Read sha256 from companion file
		shaHex := ""
		if shaPath, ok := shaMap[platformKey]; ok {
			shaData, err := os.ReadFile(shaPath)
			if err != nil {
				fatalf("read sha256 %s: %v", shaPath, err)
			}
			// Format: "<hex>  <filename>"
			shaHex = strings.Fields(string(shaData))[0]
		} else {
			fatalf("sha256 file not found for %s", platformKey)
		}

		assetName := pluginID + "_" + version + "_" + platformKey + ".zip"
		url := fmt.Sprintf("%s/releases/download/%s/%s", repo, tag, assetName)

		artifacts = append(artifacts, artifact{
			GOOS:   pattern.goos,
			GOARCH: pattern.goarch,
			URL:    url,
			SHA256: shaHex,
			Size:   info.Size(),
		})
	}

	return artifacts
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}