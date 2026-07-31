package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const abiVersion uint32 = 1

// pluginVersion is injected by CI via ldflags -X at build time.
// The fallback is the local-development default.
var pluginVersion = "0.1.2"

// MiMo Free API constants. The free tier has no API key: a bootstrap call
// exchanges a client hash for a short-lived JWT, which is then used to call
// the OpenAI-compatible chat endpoint.
const (
	mimoBaseURL      = "https://api.xiaomimimo.com"
	mimoBootstrapURL = "/api/free-ai/bootstrap"
	mimoChatURL      = "/api/free-ai/openai/chat"
	mimoSourceHeader = "mimocode-cli-free"
	sessionIDPrefix  = "ses_"
)

// mimoSystemMarker is required by MiMo's free-tier anti-abuse gate: the chat
// endpoint 403s ("Illegal access") unless a system message contains this
// exact MiMoCode signature substring.
const mimoSystemMarker = "You are MiMoCode, an interactive CLI tool that helps users with software engineering tasks."

// mimoUserAgents are rotated on every bootstrap/chat request. The anti-abuse
// gate 403s ("Illegal access") requests without a Chrome-like desktop UA.
var mimoUserAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
}

// randomUserAgent picks a random entry from mimoUserAgents.
func randomUserAgent() string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(mimoUserAgents))))
	if err != nil {
		return mimoUserAgents[0]
	}
	return mimoUserAgents[n.Int64()]
}

// JSON envelope matching plugin ABI
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Plugin metadata
type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type capabilities struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
}

// Model router RPC types. The host marshals pluginapi.ModelRouteRequest/Response
// verbatim (PascalCase Go field names, no json tags), so field names below must
// match those exactly.
type modelRouteRequest struct {
	RequestedModel string `json:"RequestedModel"`
}

type modelRouteResponse struct {
	Handled    bool   `json:"Handled"`
	TargetKind string `json:"TargetKind,omitempty"`
	Target     string `json:"Target,omitempty"`
}

// Executor RPC types
type executorRequest struct {
	AuthID          string              `json:"auth_id"`
	AuthProvider    string              `json:"auth_provider"`
	Model           string              `json:"model"`
	Format          string              `json:"format"`
	Stream          bool                `json:"stream"`
	Alt             string              `json:"alt,omitempty"`
	Headers         map[string][]string `json:"headers,omitempty"`
	SourceFormat    string              `json:"source_format"`
	Payload         json.RawMessage     `json:"payload"`
	OriginalRequest json.RawMessage     `json:"original_request"`
	StorageJSON     json.RawMessage     `json:"storage_json,omitempty"`
}

type executorStreamChunk struct {
	Payload []byte `json:"payload,omitempty"`
	Err     string `json:"error,omitempty"`
}

type executorStreamResponse struct {
	Headers map[string][]string   `json:"headers,omitempty"`
	Chunks  []executorStreamChunk `json:"chunks,omitempty"`
}

// Host HTTP bridge types
type hostHTTPRequest struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    json.RawMessage     `json:"body,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       []byte              `json:"body,omitempty"`
}

type hostHTTPStreamStart struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// requestBuf holds the raw JSON from the last host call,
// so executor methods can decode it.
var (
	mu         sync.Mutex
	requestBuf []byte
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	methodStr := C.GoString(method)

	// Capture request bytes for executor methods
	var reqBytes []byte
	if request != nil && requestLen > 0 {
		reqBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := handleMethod(methodStr, reqBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// mimoModels is the fixed set of free-tier models exposed by the MiMo Free
// bootstrap endpoint. Unlike OpenCode Free, MiMo has no discovery endpoint,
// so the list is static (mirrors PROVIDER_MODELS.mmf in the 9router config).
var mimoModels = []map[string]any{
	{"ID": "mimo-auto", "Object": "model", "OwnedBy": "mimo", "Type": "chat"},
	{"ID": "mimo-v2.5-pro", "Object": "model", "OwnedBy": "mimo", "Type": "chat"},
	{"ID": "mimo-v2.5", "Object": "model", "OwnedBy": "mimo", "Type": "chat"},
	{"ID": "mimo-v2-pro", "Object": "model", "OwnedBy": "mimo", "Type": "chat"},
	{"ID": "mimo-v2-omni", "Object": "model", "OwnedBy": "mimo", "Type": "chat"},
	{"ID": "mimo-v2-flash", "Object": "model", "OwnedBy": "mimo", "Type": "chat"},
}

// sessionID is generated once per process and sent as x-session-affinity on
// every chat request, matching the upstream 9router executor's behavior.
var sessionID = generateSessionID()

// JWT cache. The bootstrap endpoint issues short-lived tokens (~1h); we cache
// them in-memory with a 5-minute refresh buffer and invalidate on 401.
var (
	jwtMu        sync.Mutex
	cachedJWT    string
	jwtExpiresAt time.Time
)

const (
	jwtRefreshBuffer = 5 * time.Minute
	// jwtFallbackTTL is used only when the JWT's own "exp" claim can't be parsed.
	jwtFallbackTTL = 3000 * time.Second
)

type bootstrapResponse struct {
	JWT string `json:"jwt"`
}

func handleMethod(method string, reqBody []byte) ([]byte, error) {
	// Store request body for executor methods
	mu.Lock()
	requestBuf = reqBody
	mu.Unlock()

	switch method {
	case "plugin.register", "plugin.reconfigure":
		return handleRegister()
	case "model.register":
		return handleModelRegister()
	case "model.static":
		return handleModelStatic()
	case "model.for_auth":
		return handleModelForAuth()
	case "model.route":
		return handleModelRoute(reqBody)
	case "executor.identifier":
		return okEnvelopeJSON(`{"identifier":"mimo-free"}`)
	case "executor.execute":
		return handleExecute()
	case "executor.execute_stream":
		return handleExecuteStream()
	case "executor.count_tokens":
		return handleCountTokens()
	case "executor.http_request":
		return handleHTTPRequest()
	case "auth.identifier":
		return okEnvelopeJSON(`{"identifier":"mimo-free"}`)
	case "auth.parse":
		return okEnvelopeJSON(`{"handled":false}`)
	case "auth.login.start":
		return okEnvelopeJSON(`{"handled":false}`)
	case "auth.login.poll":
		return okEnvelopeJSON(`{"handled":false}`)
	case "auth.refresh":
		return okEnvelopeJSON(`{"handled":false}`)
	case "request.translate":
		return okEnvelopeJSON(`{"body":` + string(reqBody) + `}`)
	case "response.translate":
		return okEnvelopeJSON(`{"body":` + string(reqBody) + `}`)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func handleRegister() ([]byte, error) {
	reg := registration{
		SchemaVersion: 1,
		Metadata: metadata{
			Name:             "mimo-free",
			Version:          pluginVersion,
			Author:           "nhymxu",
			GitHubRepository: "https://github.com/nhymxu/cpa-plugin",
			Logo:             "",
			ConfigFields:     nil,
		},
		Capabilities: capabilities{
			ModelRegistrar:        true,
			ModelProvider:         true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		return nil, err
	}
	return okEnvelopeRaw(raw), nil
}

func handleModelRegister() ([]byte, error) {
	modelsJSON, err := json.Marshal(mimoModels)
	if err != nil {
		return nil, fmt.Errorf("model.register: %w", err)
	}
	return okEnvelopeJSON(`{"provider":"mimo-free","models":` + string(modelsJSON) + `}`)
}

func handleModelStatic() ([]byte, error) {
	return handleModelRegister()
}

func handleModelForAuth() ([]byte, error) {
	return handleModelRegister()
}

// handleModelRoute routes requests for our own free models directly to this plugin's
// executor, bypassing the host's auth scheduler. This provider has no OAuth/login flow
// and never registers a coreauth.Auth record, so without this router the scheduler would
// always fail with auth_not_found before our executor ever runs.
func handleModelRoute(reqBody []byte) ([]byte, error) {
	var req modelRouteRequest
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return nil, fmt.Errorf("decode model.route request: %w", err)
	}

	requested := strings.TrimSpace(req.RequestedModel)
	for _, m := range mimoModels {
		if id, ok := m["ID"].(string); ok && id == requested {
			resp := modelRouteResponse{Handled: true, TargetKind: "self"}
			raw, err := json.Marshal(resp)
			if err != nil {
				return nil, err
			}
			return okEnvelopeRaw(raw), nil
		}
	}

	raw, err := json.Marshal(modelRouteResponse{Handled: false})
	if err != nil {
		return nil, err
	}
	return okEnvelopeRaw(raw), nil
}

func handleExecute() ([]byte, error) {
	_, payload, err := decodeExecutorRequest()
	if err != nil {
		return nil, fmt.Errorf("decode execute request: %w", err)
	}
	payload = injectSystemMarker(payload)

	jwt, err := bootstrapJWT()
	if err != nil {
		return nil, fmt.Errorf("mimo-free bootstrap: %w", err)
	}

	sendReq := hostHTTPRequest{
		Method:  "POST",
		URL:     mimoBaseURL + mimoChatURL,
		Headers: buildChatHeaders(jwt, false),
		Body:    payload,
	}

	resp, err := callHostHTTP(sendReq)
	if err != nil {
		return nil, fmt.Errorf("mimo-free request failed: %w", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		invalidateJWT()
		jwt, err = bootstrapJWT()
		if err != nil {
			return nil, fmt.Errorf("mimo-free re-bootstrap: %w", err)
		}
		sendReq.Headers = buildChatHeaders(jwt, false)
		resp, err = callHostHTTP(sendReq)
		if err != nil {
			return nil, fmt.Errorf("mimo-free retry request failed: %w", err)
		}
	}

	b64Body := base64.StdEncoding.EncodeToString(resp.Body)
	return okEnvelopeJSON(`{"payload":"` + b64Body + `","headers":{"content-type":["application/json"]}}`)
}

func handleExecuteStream() ([]byte, error) {
	_, payload, err := decodeExecutorRequest()
	if err != nil {
		return nil, fmt.Errorf("decode execute_stream request: %w", err)
	}
	payload = injectSystemMarker(payload)

	jwt, err := bootstrapJWT()
	if err != nil {
		return nil, fmt.Errorf("mimo-free bootstrap: %w", err)
	}

	sendReq := hostHTTPRequest{
		Method:  "POST",
		URL:     mimoBaseURL + mimoChatURL,
		Headers: buildChatHeaders(jwt, true),
		Body:    payload,
	}

	start, err := startHostStream(sendReq)
	if err != nil {
		return nil, fmt.Errorf("mimo-free stream failed: %w", err)
	}

	if start.StatusCode == 401 || start.StatusCode == 403 {
		closeHostStream(start.StreamID)
		invalidateJWT()
		jwt, err = bootstrapJWT()
		if err != nil {
			return nil, fmt.Errorf("mimo-free re-bootstrap: %w", err)
		}
		sendReq.Headers = buildChatHeaders(jwt, true)
		start, err = startHostStream(sendReq)
		if err != nil {
			return nil, fmt.Errorf("mimo-free stream retry failed: %w", err)
		}
	}

	return collectStream(start.StreamID)
}

func handleCountTokens() ([]byte, error) {
	return okEnvelopeJSON(`{"payload":"eyJ0b3RhbF90b2tlbnMiOjB9"}`)
}

func handleHTTPRequest() ([]byte, error) {
	return okEnvelopeJSON(`{"status_code":200,"headers":{"content-type":["application/json"]},"body":"eyJzdGF0dXMiOiJtaW1vLWZyZWUifQ=="}`)
}

// buildChatHeaders builds the headers MiMo's free-tier chat endpoint expects,
// mirroring the upstream 9router executor (X-Mimo-Source + session affinity
// + rotating Chrome-like User-Agent to pass the anti-abuse gate).
func buildChatHeaders(jwt string, stream bool) map[string][]string {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	return map[string][]string{
		"Content-Type":       {"application/json"},
		"X-Mimo-Source":      {mimoSourceHeader},
		"User-Agent":         {randomUserAgent()},
		"x-session-affinity": {sessionID},
		"Accept":             {accept},
		"Authorization":      {"Bearer " + jwt},
	}
}

// bootstrapJWT returns a cached JWT if still valid, otherwise exchanges a
// fresh machine fingerprint for a new one via the free-ai bootstrap endpoint.
func bootstrapJWT() (string, error) {
	jwtMu.Lock()
	if cachedJWT != "" && time.Now().Before(jwtExpiresAt.Add(-jwtRefreshBuffer)) {
		jwt := cachedJWT
		jwtMu.Unlock()
		return jwt, nil
	}
	jwtMu.Unlock()

	body, err := json.Marshal(map[string]string{"client": generateFingerprint()})
	if err != nil {
		return "", err
	}

	resp, err := callHostHTTP(hostHTTPRequest{
		Method: "POST",
		URL:    mimoBaseURL + mimoBootstrapURL,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"User-Agent":   {randomUserAgent()},
		},
		Body: body,
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("bootstrap failed: status %d", resp.StatusCode)
	}

	var data bootstrapResponse
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return "", fmt.Errorf("decode bootstrap response: %w", err)
	}
	if data.JWT == "" {
		return "", fmt.Errorf("bootstrap returned no jwt")
	}

	expiresAt, ok := parseJWTExpiry(data.JWT)
	if !ok {
		expiresAt = time.Now().Add(jwtFallbackTTL)
	}

	jwtMu.Lock()
	cachedJWT = data.JWT
	jwtExpiresAt = expiresAt
	jwtMu.Unlock()

	return data.JWT, nil
}

func invalidateJWT() {
	jwtMu.Lock()
	cachedJWT = ""
	jwtExpiresAt = time.Time{}
	jwtMu.Unlock()
}

// parseJWTExpiry decodes the "exp" claim (unix seconds) from a JWT's payload
// segment. Returns ok=false if the token can't be parsed.
func parseJWTExpiry(jwtToken string) (time.Time, bool) {
	parts := strings.Split(jwtToken, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// generateFingerprint mirrors the upstream JS executor's stable per-machine
// bootstrap "client" identifier. A value that changes on every call looked
// bot-like to MiMo's anti-abuse gate, so this is deliberately stable across
// requests/restarts on the same host. Go has no portable CPU-model API, so
// NumCPU stands in for the JS version's CPU model string — the field only
// needs to be a stable-ish per-machine identifier, not an exact replica.
func generateFingerprint() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	username := "unknown-user"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}
	seed := fmt.Sprintf("%s|%s|%s|%d|%s", hostname, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), username)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// injectSystemMarker ensures the outgoing chat payload carries the anti-abuse
// marker in a system message (idempotent). Best-effort: any decode failure
// leaves the payload untouched rather than failing the request.
func injectSystemMarker(payload json.RawMessage) json.RawMessage {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload
	}
	rawMessages, ok := body["messages"]
	if !ok {
		return payload
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return payload
	}

	for _, raw := range messages {
		var m struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err == nil && m.Role == "system" && strings.Contains(m.Content, mimoSystemMarker) {
			return payload
		}
	}

	markerMsg, err := json.Marshal(map[string]string{"role": "system", "content": mimoSystemMarker})
	if err != nil {
		return payload
	}
	messages = append([]json.RawMessage{markerMsg}, messages...)

	newMessagesJSON, err := json.Marshal(messages)
	if err != nil {
		return payload
	}
	body["messages"] = newMessagesJSON

	newPayload, err := json.Marshal(body)
	if err != nil {
		return payload
	}
	return newPayload
}

// generateSessionID mirrors the upstream JS executor's per-process
// "ses_" + 24 random lowercase-alphanumeric characters session ID.
func generateSessionID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	sb.WriteString(sessionIDPrefix)
	for i := 0; i < 24; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			sb.WriteByte(chars[0])
			continue
		}
		sb.WriteByte(chars[n.Int64()])
	}
	return sb.String()
}

// decodeExecutorRequest parses the last stored request buffer into executor types.
func decodeExecutorRequest() (*executorRequest, json.RawMessage, error) {
	mu.Lock()
	buf := requestBuf
	mu.Unlock()

	if len(buf) == 0 {
		return nil, nil, fmt.Errorf("no request buffer available")
	}

	var req executorRequest
	if err := json.Unmarshal(buf, &req); err != nil {
		return nil, nil, fmt.Errorf("unmarshal executor request: %w", err)
	}

	// Prefer the translated payload; fall back to the original request.
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	return &req, payload, nil
}

// callHostHTTP sends a request through the host HTTP bridge (non-streaming).
func callHostHTTP(req hostHTTPRequest) (*hostHTTPResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result, err := hostCall("host.http.do", raw)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// startHostStream opens a streaming HTTP call through the host bridge and
// returns the initial status/headers without consuming the body, so callers
// can decide whether to retry (e.g. on a 401) before reading any chunks.
func startHostStream(req hostHTTPRequest) (*hostHTTPStreamStart, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result, err := hostCall("host.http.do_stream", raw)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPStreamStart
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	if resp.StreamID == "" {
		return nil, fmt.Errorf("host returned empty stream id")
	}
	return &resp, nil
}

// collectStream drains a previously-started host stream into chunks.
func collectStream(streamID string) ([]byte, error) {
	var chunks []executorStreamChunk
	for {
		readResp, errRead := readHostStream(streamID)
		if errRead != nil {
			closeHostStream(streamID)
			return nil, fmt.Errorf("stream read: %w", errRead)
		}
		if readResp.Error != "" {
			chunks = append(chunks, executorStreamChunk{Err: readResp.Error})
		} else if len(readResp.Payload) > 0 {
			chunks = append(chunks, executorStreamChunk{Payload: readResp.Payload})
		}
		if readResp.Done {
			break
		}
	}
	closeHostStream(streamID)

	out := executorStreamResponse{
		Headers: map[string][]string{"content-type": {"text/event-stream"}},
		Chunks:  chunks,
	}
	outRaw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return okEnvelopeRaw(outRaw), nil
}

func readHostStream(streamID string) (*hostHTTPStreamReadResponse, error) {
	req := map[string]string{"stream_id": streamID}
	raw, _ := json.Marshal(req)
	result, err := hostCall("host.http.stream_read", raw)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPStreamReadResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func closeHostStream(streamID string) error {
	req := map[string]string{"stream_id": streamID}
	raw, _ := json.Marshal(req)
	_, err := hostCall("host.http.stream_close", raw)
	return err
}

// hostCall sends a JSON-RPC-style method call to the host and returns the result field.
func hostCall(method string, payload []byte) (json.RawMessage, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}

	var response C.cliproxy_buffer
	ret := C.call_host_api(cMethod, req, C.size_t(len(payload)), &response)
	if ret != 0 {
		if response.ptr != nil {
			C.free_host_buffer(response.ptr, response.len)
		}
		return nil, fmt.Errorf("host call '%s' returned code %d", method, int(ret))
	}
	if response.ptr == nil || response.len == 0 {
		return json.RawMessage("{}"), nil
	}

	raw := C.GoBytes(response.ptr, C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host '%s' response: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("host '%s' error: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host '%s' returned ok=false", method)
	}
	return env.Result, nil
}

// Envelope helpers
func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
}

func okEnvelopeRaw(result []byte) []byte {
	raw, _ := json.Marshal(envelope{OK: true, Result: result})
	return raw
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
