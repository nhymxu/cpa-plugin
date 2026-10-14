package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustIntercept(t *testing.T, headers map[string][]string) interceptResponse {
	t.Helper()
	return mustInterceptWithMeta(t, headers, nil)
}

func mustInterceptWithMeta(t *testing.T, headers map[string][]string, meta map[string]any) interceptResponse {
	t.Helper()
	raw, err := json.Marshal(interceptRequest{RequestID: "test", Headers: headers, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	out, err := interceptHeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", out)
	}
	var resp interceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNilHeadersNoop(t *testing.T) {
	resp := mustIntercept(t, nil)
	if len(resp.Headers) != 0 {
		t.Fatalf("headers = %v, want empty", resp.Headers)
	}
}

func TestExistingOpencodeSessionPreserved(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{
		"x-opencode-session": {"ses_client"},
		"Session-Id":         {"should-not-win"},
	})
	if len(resp.Headers) != 0 {
		t.Fatalf("headers = %v, want empty (client value authoritative)", resp.Headers)
	}
}

func TestConflictingCaseVariantSessionHeadersFailClosed(t *testing.T) {
	resp := mustInterceptWithMeta(t,
		map[string][]string{
			"Session-Id": {"session-a"},
			"session-id": {"session-b"},
		},
		map[string]any{"canonical_session_id": "codex:fallback-must-not-win"})
	if len(resp.Headers) != 0 {
		t.Fatalf("headers = %v, want empty for ambiguous session headers", resp.Headers)
	}
}

func TestRepeatedEquivalentSessionHeadersAccepted(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{
		"Session-Id": {"session-a", " session-a "},
		"session-id": {"session-a"},
	})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "session-a" {
		t.Fatalf("headers = %v, want normalized session-a", resp.Headers)
	}
}

func TestInvalidSessionHeadersFailClosed(t *testing.T) {
	invalid := []string{
		"contains\rreturn",
		"contains\nnewline",
		"contains\x00nul",
		"contains" + string(rune(0x85)) + "control",
		strings.Repeat("x", maxSessionIDBytes+1),
	}
	for _, value := range invalid {
		resp := mustInterceptWithMeta(t,
			map[string][]string{
				"Session-Id":   {value},
				"X-Session-Id": {"lower-priority-must-not-win"},
			},
			map[string]any{"canonical_session_id": "codex:fallback-must-not-win"})
		if len(resp.Headers) != 0 {
			t.Fatalf("value %q: headers = %v, want empty", value, resp.Headers)
		}
	}
}

func TestCodexSessionMapped(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{"Session-Id": {"codex-ses-1"}})
	got := resp.Headers["X-Opencode-Session"]
	if len(got) != 1 || got[0] != "codex-ses-1" {
		t.Fatalf("headers = %v, want x-opencode-session=codex-ses-1", resp.Headers)
	}
	if got := resp.Headers["X-Opencode-Client"]; len(got) != 1 || got[0] != "cliproxy" {
		t.Fatalf("headers = %v, want x-opencode-client=cliproxy", resp.Headers)
	}
}

func TestUnderscoreVariantsMapped(t *testing.T) {
	for _, h := range []string{"Session_id", "Thread-Id", "Thread_id", "X-Session-Affinity", "X-Session-Id"} {
		resp := mustIntercept(t, map[string][]string{h: {"v-" + h}})
		if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "v-"+h {
			t.Fatalf("%s: headers = %v", h, resp.Headers)
		}
	}
}

func TestClaudeCodeSessionMapped(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{"X-Claude-Code-Session-Id": {"cc-123"}})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "cc-123" {
		t.Fatalf("headers = %v", resp.Headers)
	}
}

func TestDshSessionMapped(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{"X-DeepSeek-Harness-Session-Id": {"dsh-9"}})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "dsh-9" {
		t.Fatalf("headers = %v", resp.Headers)
	}
}

func TestDshSessionAffinityMapped(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{"X-Session-Affinity": {"ses_affinity_1"}})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "ses_affinity_1" {
		t.Fatalf("headers = %v", resp.Headers)
	}
}

func TestClientHeaderPreserved(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{
		"Session-Id":        {"s1"},
		"x-opencode-client": {"opencode"},
	})
	if _, ok := resp.Headers["X-Opencode-Client"]; ok {
		t.Fatalf("headers = %v, want client identity preserved", resp.Headers)
	}
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "s1" {
		t.Fatalf("headers = %v", resp.Headers)
	}
}

func TestBlankClientHeaderGetsDefault(t *testing.T) {
	cases := []struct {
		name   string
		values []string
	}{
		{name: "empty list", values: []string{}},
		{name: "blank value", values: []string{"  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustIntercept(t, map[string][]string{
				"Session-Id":        {"s1"},
				"x-opencode-client": tc.values,
			})
			if got := resp.Headers["X-Opencode-Client"]; len(got) != 1 || got[0] != defaultClientValue {
				t.Fatalf("headers = %v, want default client identity", resp.Headers)
			}
		})
	}
}

func TestClientRequestIDMappedAsLastResort(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{"X-Client-Request-Id": {"req-1"}})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "req-1" {
		t.Fatalf("headers = %v, want x-opencode-session=req-1", resp.Headers)
	}
}

func TestSessionHeaderBeatsClientRequestID(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{
		"X-Client-Request-Id": {"per-call"},
		"Session-Id":          {"codex-ses-1"},
	})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "codex-ses-1" {
		t.Fatalf("headers = %v, want the stable session id to win", resp.Headers)
	}
}

func TestCanonicalSessionFallback(t *testing.T) {
	resp := mustInterceptWithMeta(t,
		map[string][]string{"User-Agent": {"deepseek-harness/0.1.5-rc.1"}},
		map[string]any{"canonical_session_id": "codex:probe-capture-1"})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "codex:probe-capture-1" {
		t.Fatalf("headers = %v, want fallback session id", resp.Headers)
	}
}

func TestInvalidCanonicalSessionFallbackIgnored(t *testing.T) {
	for _, value := range []string{"contains\nnewline", strings.Repeat("x", maxSessionIDBytes+1)} {
		resp := mustInterceptWithMeta(t,
			map[string][]string{"User-Agent": {"deepseek-harness/0.1.5-rc.1"}},
			map[string]any{"canonical_session_id": value})
		if len(resp.Headers) != 0 {
			t.Fatalf("value %q: headers = %v, want empty", value, resp.Headers)
		}
	}
}

func TestClientHeaderBeatsFallback(t *testing.T) {
	resp := mustInterceptWithMeta(t,
		map[string][]string{"Session-Id": {"codex-ses-1"}},
		map[string]any{"canonical_session_id": "codex:should-not-win"})
	if got := resp.Headers["X-Opencode-Session"]; len(got) != 1 || got[0] != "codex-ses-1" {
		t.Fatalf("headers = %v, want client header to win", resp.Headers)
	}
}

func TestNoFallbackWithoutMetadata(t *testing.T) {
	resp := mustIntercept(t, map[string][]string{"User-Agent": {"deepseek-harness/0.1.5-rc.1"}})
	if len(resp.Headers) != 0 {
		t.Fatalf("headers = %v, want empty when no metadata and no source header", resp.Headers)
	}
}

func TestFallbackIgnoresMalformedMetadata(t *testing.T) {
	for _, meta := range []map[string]any{
		{"canonical_session_id": ""},
		{"canonical_session_id": "   "},
		{"canonical_session_id": 42},
		{"canonical_session_id": nil},
		{"other_key": "x"},
	} {
		resp := mustInterceptWithMeta(t, map[string][]string{"User-Agent": {"dsh"}}, meta)
		if len(resp.Headers) != 0 {
			t.Fatalf("meta %v: headers = %v, want empty", meta, resp.Headers)
		}
	}
}

func TestRegister(t *testing.T) {
	out, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("register not ok: %s", out)
	}
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != abiVersion {
		t.Fatalf("schema version = %d, want %d", reg.SchemaVersion, abiVersion)
	}
	if !reg.Capabilities.RequestInterceptor {
		t.Fatal("want request_interceptor capability")
	}
	if !reg.Capabilities.Executor {
		t.Fatal("want executor capability")
	}
	if !reg.Capabilities.ModelRegistrar {
		t.Fatal("want model_registrar capability")
	}
	if !reg.Capabilities.ModelProvider {
		t.Fatal("want model_provider capability")
	}
	if !reg.Capabilities.ModelRouter {
		t.Fatal("want model_router capability")
	}
	if reg.Metadata.Name != "opencode-free" ||
		reg.Metadata.Version != pluginVersion ||
		reg.Metadata.Author != "nhymxu" ||
		reg.Metadata.GitHubRepository != "https://github.com/nhymxu/cpa-plugin" {
		t.Fatalf("metadata = %+v, want current release identity", reg.Metadata)
	}
}

func TestRequestPayloadSizeAllowed(t *testing.T) {
	for _, size := range []uint64{0, maxRequestPayloadBytes - 1, maxRequestPayloadBytes} {
		if !requestPayloadSizeAllowed(size) {
			t.Fatalf("payload size %d should be accepted", size)
		}
	}
	for _, size := range []uint64{maxRequestPayloadBytes + 1, ^uint64(0)} {
		if requestPayloadSizeAllowed(size) {
			t.Fatalf("payload size %d should be rejected", size)
		}
	}
}

func TestProcessPluginCallReturnsFailureEnvelope(t *testing.T) {
	raw, status := processPluginCall("request.intercept_after", []byte(`{"Headers":`))
	if status != 1 {
		t.Fatalf("status = %d, want 1", status)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "plugin_error" {
		t.Fatalf("envelope = %+v, want plugin_error", env)
	}
}

func TestHostShapedLargeBodyPayloadMapped(t *testing.T) {
	const sessionFixture = "123e4567-e89b-12d3-a456-426614174000"
	for _, method := range []string{"request.intercept_before", "request.intercept_after"} {
		raw, err := json.Marshal(struct {
			RequestID string              `json:"RequestID"`
			TraceID   string              `json:"TraceID"`
			Source    string              `json:"SourceFormat"`
			Headers   map[string][]string `json:"Headers"`
			Metadata  map[string]any      `json:"Metadata"`
			Body      []byte              `json:"Body"`
		}{
			RequestID: "large-body",
			TraceID:   "trace",
			Source:    "claude",
			Headers:   map[string][]string{"X-Claude-Code-Session-Id": {sessionFixture}},
			Body:      []byte(strings.Repeat("a", 6378698)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) <= 8<<20 {
			t.Fatal("fixture does not exceed the previous payload gate")
		}
		if !requestPayloadSizeAllowed(uint64(len(raw))) {
			t.Fatal("host-shaped envelope rejected by payload gate")
		}
		out, err := handleMethod(method, raw)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatal("host-shaped envelope not accepted")
		}
		var resp interceptResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		got := resp.Headers["X-Opencode-Session"]
		if len(got) != 1 || got[0] != sessionFixture {
			t.Fatal("host-shaped envelope session not mapped")
		}
		client := resp.Headers["X-Opencode-Client"]
		if len(client) != 1 || client[0] != defaultClientValue {
			t.Fatal("host-shaped envelope client header not mapped")
		}
	}
}

func TestClaudeCodeUUIDSessionMappedBothMethods(t *testing.T) {
	const sessionFixture = "123e4567-e89b-12d3-a456-426614174000"
	if len(sessionFixture) != 36 {
		t.Fatal("session fixture must be 36 bytes")
	}
	for _, method := range []string{"request.intercept_before", "request.intercept_after"} {
		raw, err := json.Marshal(interceptRequest{
			RequestID: "uuid-shape",
			Headers:   map[string][]string{"X-Claude-Code-Session-Id": {sessionFixture}},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := handleMethod(method, raw)
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatal(err)
		}
		if !env.OK {
			t.Fatal("uuid-shaped session envelope not accepted")
		}
		var resp interceptResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatal(err)
		}
		got := resp.Headers["X-Opencode-Session"]
		if len(got) != 1 || got[0] != sessionFixture {
			t.Fatal("uuid-shaped session not mapped")
		}
	}
}

// Outbound header tests for executor execution paths
func TestBuildOutboundHeadersWithExistingSession(t *testing.T) {
	headers := map[string][]string{
		"X-Opencode-Session": {"custom-session-123"},
	}
	out := buildOutboundHeaders(headers, nil, "application/json")
	if got := out["X-Opencode-Session"]; len(got) != 1 || got[0] != "custom-session-123" {
		t.Fatalf("outbound session = %v, want custom-session-123", got)
	}
	if got := out["X-Opencode-Client"]; len(got) != 1 || got[0] != "desktop" {
		t.Fatalf("outbound client = %v, want desktop", got)
	}
	if got := out["Authorization"]; len(got) != 1 || got[0] != "Bearer public" {
		t.Fatalf("outbound auth = %v, want Bearer public", got)
	}
	if got := out["Accept"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("outbound accept = %v, want application/json", got)
	}
}

func TestBuildOutboundHeadersWithSessionSources(t *testing.T) {
	headers := map[string][]string{
		"Session-Id": {"codex-session-abc"},
	}
	out := buildOutboundHeaders(headers, nil, "text/event-stream")
	if got := out["X-Opencode-Session"]; len(got) != 1 || got[0] != "codex-session-abc" {
		t.Fatalf("outbound session = %v, want codex-session-abc", got)
	}
	if got := out["Accept"]; len(got) != 1 || got[0] != "text/event-stream" {
		t.Fatalf("outbound accept = %v, want text/event-stream", got)
	}
}

func TestBuildOutboundHeadersFallbackGenerated(t *testing.T) {
	out := buildOutboundHeaders(nil, nil, "application/json")
	got := out["X-Opencode-Session"]
	if len(got) != 1 || !strings.HasPrefix(got[0], "cpa-") {
		t.Fatalf("outbound session = %v, want generated cpa- prefix", got)
	}
}

func TestBuildOutboundHeadersClientPreserved(t *testing.T) {
	headers := map[string][]string{
		"X-Opencode-Client": {"my-custom-client"},
	}
	out := buildOutboundHeaders(headers, nil, "application/json")
	if got := out["X-Opencode-Client"]; len(got) != 1 || got[0] != "my-custom-client" {
		t.Fatalf("outbound client = %v, want my-custom-client", got)
	}
}
