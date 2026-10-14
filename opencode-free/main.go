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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"
)

const abiVersion uint32 = 1

// pluginVersion is injected by CI via ldflags -X at build time.
// The fallback is the local-development default.
var pluginVersion = "0.1.8"

// OpenCode API constants
const (
	opencodeBaseURL     = "https://opencode.ai"
	opencodeModelsURL   = "/zen/v1/models"
	opencodeChatURL     = "/zen/v1/chat/completions"
	opencodeMessagesURL = "/zen/v1/messages"
)

// Session mapping constants
const (
	maxRequestPayloadBytes = 64 << 20
	maxSessionIDBytes      = 1024

	targetSessionHeader = "X-Opencode-Session"
	targetClientHeader  = "X-Opencode-Client"
	defaultClientValue  = "cliproxy"
)

// sessionSources lists downstream client headers that carry a stable
// per-conversation session id, in priority order.
var sessionSources = []string{
	"Session-Id",                    // codex CLI
	"Session_id",                    // codex CLI underscore variant (EqualFold misses _ vs -)
	"Thread-Id",                     // codex CLI thread = conversation
	"Thread_id",                     // underscore variant
	"X-Claude-Code-Session-Id",      // claude code
	"X-DeepSeek-Harness-Session-Id", // dsh / deepseek-harness native provider
	"X-Session-Affinity",            // dsh pi-ai (anthropic-messages, openai-completions)
	"X-Session-Id",                  // generic (opencode-ish clients, aicoding-proxy default)
	"X-Client-Request-Id",           // last resort
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
	RequestInterceptor    bool     `json:"request_interceptor"`
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
	Metadata        map[string]any      `json:"metadata,omitempty"`
}

type executorStreamChunk struct {
	Payload []byte `json:"payload,omitempty"`
	Err     string `json:"error,omitempty"`
}

type executorStreamResponse struct {
	Headers map[string][]string   `json:"headers,omitempty"`
	Chunks  []executorStreamChunk `json:"chunks,omitempty"`
}

// Interceptor types
type interceptRequest struct {
	RequestID string              `json:"RequestID"`
	Headers   map[string][]string `json:"Headers"`
	Metadata  map[string]any      `json:"Metadata"`
}

type interceptResponse struct {
	Headers      map[string][]string `json:"Headers"`
	ClearHeaders []string            `json:"ClearHeaders"`
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

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// requestBuffer holds the raw JSON from the last host call,
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
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	status = 1
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, errorEnvelope("plugin_panic", "plugin call failed"))
			status = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	if !requestPayloadSizeAllowed(uint64(requestLen)) {
		writeResponse(response, errorEnvelope("invalid_request", "request payload exceeds size limit"))
		return 1
	}
	var payload []byte
	if requestLen > 0 {
		if request == nil {
			writeResponse(response, errorEnvelope("invalid_request", "request buffer is required"))
			return 1
		}
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, callStatus := processPluginCall(C.GoString(method), payload)
	writeResponse(response, raw)
	return C.int(callStatus)
}

func processPluginCall(method string, payload []byte) ([]byte, int) {
	raw, errHandle := handleMethod(method, payload)
	if errHandle != nil {
		return errorEnvelope("plugin_error", errHandle.Error()), 1
	}
	return raw, 0
}

func requestPayloadSizeAllowed(size uint64) bool {
	return size <= maxRequestPayloadBytes
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

// Free model IDs that don't end with "-free" suffix
var knownFreeModels = map[string]bool{
	"big-pickle": true,
}

// model cache with 10-minute TTL
var (
	cachedModels []map[string]any
	cacheTime    time.Time
	cacheMu      sync.Mutex
)

const modelCacheTTL = 10 * time.Minute

// opencodeModel is a model from the OpenCode /zen/v1/models endpoint.
type opencodeModel struct {
	ID string `json:"id"`
}

// opencodeModelsResponse wraps the /zen/v1/models list payload: {"object":"list","data":[...]}
type opencodeModelsResponse struct {
	Data []opencodeModel `json:"data"`
}

// fetchFreeModels fetches models from OpenCode and filters only free ones.
// Results are cached for 10 minutes to avoid excessive API calls.
func fetchFreeModels() ([]map[string]any, error) {
	cacheMu.Lock()
	if cachedModels != nil && time.Since(cacheTime) < modelCacheTTL {
		models := cachedModels
		cacheMu.Unlock()
		return models, nil
	}
	cacheMu.Unlock()

	sendReq := hostHTTPRequest{
		Method: "GET",
		URL:    opencodeBaseURL + opencodeModelsURL,
		Headers: map[string][]string{
			"Authorization":     {"Bearer public"},
			"x-opencode-client": {"desktop"},
			"Accept":            {"application/json"},
		},
	}

	resp, err := callHostHTTP(sendReq)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}

	var modelsResp opencodeModelsResponse
	if err := json.Unmarshal(resp.Body, &modelsResp); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}

	out := make([]map[string]any, 0)
	for _, m := range modelsResp.Data {
		if !strings.HasSuffix(m.ID, "-free") && !knownFreeModels[m.ID] {
			continue
		}
		out = append(out, map[string]any{
			"ID":      m.ID,
			"Object":  "model",
			"OwnedBy": "opencode",
			"Type":    "chat",
		})
	}

	cacheMu.Lock()
	cachedModels = out
	cacheTime = time.Now()
	cacheMu.Unlock()

	return out, nil
}

// Session resolution helpers
func headerValue(headers map[string][]string, name string) (string, bool) {
	matched := false
	selected := ""
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		matched = true
		if len(values) == 0 {
			return "", true
		}
		for _, raw := range values {
			value, valid := normalizeSessionID(raw)
			if !valid {
				return "", true
			}
			if selected == "" {
				selected = value
				continue
			}
			if selected != value {
				return "", true
			}
		}
	}
	if !matched {
		return "", false
	}
	return selected, true
}

func headerHasValue(headers map[string][]string, name string) bool {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func normalizeSessionID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxSessionIDBytes {
		return "", false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return "", false
		}
	}
	return value, true
}

func sessionFallback(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	v, ok := metadata["canonical_session_id"]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	value, valid := normalizeSessionID(s)
	if !valid {
		return ""
	}
	return value
}

func resolveSession(headers map[string][]string, metadata map[string]any) string {
	if headers != nil {
		// Client already sent x-opencode-session (e.g. real opencode CLI):
		// never override, their value is authoritative.
		if v, exists := headerValue(headers, targetSessionHeader); exists && v != "" {
			return v
		}
		for _, src := range sessionSources {
			if v, exists := headerValue(headers, src); exists {
				if v == "" {
					return ""
				}
				return v
			}
		}
	}
	if metadata != nil {
		if s := sessionFallback(metadata); s != "" {
			return s
		}
	}
	return ""
}

func generateFallbackSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("cpa-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("cpa-%x", b)
}

func interceptHeaders(payload []byte) ([]byte, error) {
	empty, errEmpty := okEnvelopeJSON(`{"Headers":{}}`)
	if errEmpty != nil {
		return nil, errEmpty
	}
	var req interceptRequest
	if len(payload) > 0 {
		if errDecode := json.Unmarshal(payload, &req); errDecode != nil {
			return nil, errDecode
		}
	}
	if req.Headers == nil {
		return empty, nil
	}
	// Client already sent x-opencode-session (e.g. real opencode CLI):
	// never override, their value is authoritative.
	if _, exists := headerValue(req.Headers, targetSessionHeader); exists {
		return empty, nil
	}
	var session string
	for _, src := range sessionSources {
		if v, exists := headerValue(req.Headers, src); exists {
			if v == "" {
				return empty, nil
			}
			session = v
			break
		}
	}
	if session == "" {
		session = sessionFallback(req.Metadata)
	}
	if session == "" {
		return empty, nil
	}
	out := map[string][]string{targetSessionHeader: {session}}
	// Identify the proxy only when the client didn't identify itself.
	if !headerHasValue(req.Headers, targetClientHeader) {
		out[targetClientHeader] = []string{defaultClientValue}
	}
	resp := interceptResponse{Headers: out}
	respBytes, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return okEnvelopeRaw(respBytes), nil
}

func buildOutboundHeaders(headers map[string][]string, metadata map[string]any, accept string) map[string][]string {
	out := map[string][]string{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer public"},
		"Accept":        {accept},
	}

	// Resolve x-opencode-client
	if headers != nil && headerHasValue(headers, targetClientHeader) {
		for key, values := range headers {
			if strings.EqualFold(key, targetClientHeader) {
				for _, v := range values {
					if trimmed := strings.TrimSpace(v); trimmed != "" {
						out[targetClientHeader] = []string{trimmed}
						break
					}
				}
				break
			}
		}
	}
	if _, ok := out[targetClientHeader]; !ok {
		out[targetClientHeader] = []string{"desktop"}
	}

	// Resolve x-opencode-session
	session := resolveSession(headers, metadata)
	if session == "" {
		session = generateFallbackSessionID()
	}
	if session != "" {
		out[targetSessionHeader] = []string{session}
	}

	return out
}

func handleMethod(method string, reqBody []byte) ([]byte, error) {
	// Store request body for executor methods
	mu.Lock()
	requestBuf = reqBody
	mu.Unlock()

	switch method {
	case "plugin.register", "plugin.reconfigure":
		return handleRegister()
	case "request.intercept_before", "request.intercept_after":
		return interceptHeaders(reqBody)
	case "model.register":
		return handleModelRegister()
	case "model.static":
		return handleModelStatic()
	case "model.for_auth":
		return handleModelForAuth()
	case "model.route":
		return handleModelRoute(reqBody)
	case "executor.identifier":
		return okEnvelopeJSON(`{"identifier":"opencode-free"}`)
	case "executor.execute":
		return handleExecute()
	case "executor.execute_stream":
		return handleExecuteStream()
	case "executor.count_tokens":
		return handleCountTokens()
	case "executor.http_request":
		return handleHTTPRequest()
	case "auth.identifier":
		return okEnvelopeJSON(`{"identifier":"opencode-free"}`)
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
			Name:             "opencode-free",
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
			RequestInterceptor:    true,
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
	models, err := fetchFreeModels()
	if err != nil {
		return nil, fmt.Errorf("model.register: %w", err)
	}
	modelsJSON, _ := json.Marshal(models)
	return okEnvelopeJSON(`{"provider":"opencode-free","models":` + string(modelsJSON) + `}`)
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

	models, err := fetchFreeModels()
	if err != nil {
		return nil, fmt.Errorf("model.route: %w", err)
	}

	requested := strings.TrimSpace(req.RequestedModel)
	for _, m := range models {
		if id, ok := m["ID"].(string); ok && id == requested {
			resp := modelRouteResponse{Handled: true, TargetKind: "self"}
			raw, errMarshal := json.Marshal(resp)
			if errMarshal != nil {
				return nil, errMarshal
			}
			return okEnvelopeRaw(raw), nil
		}
	}

	resp, errMarshal := json.Marshal(modelRouteResponse{Handled: false})
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelopeRaw(resp), nil
}

func handleExecute() ([]byte, error) {
	req, payload, err := decodeExecutorRequest()
	if err != nil {
		return nil, fmt.Errorf("decode execute request: %w", err)
	}

	chatURL := selectChatURL(req.Format, req.SourceFormat)

	outHeaders := buildOutboundHeaders(req.Headers, req.Metadata, "application/json")

	sendReq := hostHTTPRequest{
		Method:  "POST",
		URL:     opencodeBaseURL + chatURL,
		Headers: outHeaders,
		Body:    payload,
	}

	resp, err := callHostHTTP(sendReq)
	if err != nil {
		return nil, fmt.Errorf("opencode-free request failed: %w", err)
	}

	b64Body := base64.StdEncoding.EncodeToString(resp.Body)
	return okEnvelopeJSON(`{"payload":"` + b64Body + `","headers":{"content-type":["application/json"]}}`)
}

func handleExecuteStream() ([]byte, error) {
	req, payload, err := decodeExecutorRequest()
	if err != nil {
		return nil, fmt.Errorf("decode execute_stream request: %w", err)
	}

	chatURL := selectChatURL(req.Format, req.SourceFormat)

	outHeaders := buildOutboundHeaders(req.Headers, req.Metadata, "text/event-stream")

	sendReq := hostHTTPRequest{
		Method:  "POST",
		URL:     opencodeBaseURL + chatURL,
		Headers: outHeaders,
		Body:    payload,
	}

	return doStream(sendReq)
}

func handleCountTokens() ([]byte, error) {
	return okEnvelopeJSON(`{"payload":"eyJ0b3RhbF90b2tlbnMiOjB9"}`)
}

func handleHTTPRequest() ([]byte, error) {
	return okEnvelopeJSON(`{"status_code":200,"headers":{"content-type":["application/json"]},"body":"eyJzdGF0dXMiOiJvcGVuY29kZS1mcmVlIn0="}`)
}

// selectChatURL returns /zen/v1/messages for Claude format, /zen/v1/chat/completions otherwise.
func selectChatURL(format, sourceFormat string) string {
	if format == "claude" || sourceFormat == "claude" {
		return opencodeMessagesURL
	}
	return opencodeChatURL
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

// doStream makes a streaming HTTP call through the host bridge and collects all chunks.
func doStream(req hostHTTPRequest) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result, err := hostCall("host.http.do_stream", raw)
	if err != nil {
		return nil, err
	}

	var streamResp struct {
		StatusCode int                 `json:"status_code"`
		Headers    map[string][]string `json:"headers,omitempty"`
		StreamID   string              `json:"stream_id"`
	}
	if err := json.Unmarshal(result, &streamResp); err != nil {
		return nil, err
	}
	if streamResp.StreamID == "" {
		return nil, fmt.Errorf("host returned empty stream id")
	}

	var chunks []executorStreamChunk
	for {
		readResp, errRead := readHostStream(streamResp.StreamID)
		if errRead != nil {
			closeHostStream(streamResp.StreamID)
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
	closeHostStream(streamResp.StreamID)

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
