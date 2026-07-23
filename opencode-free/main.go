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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"
)

const abiVersion uint32 = 1

// OpenCode API constants
const (
	opencodeBaseURL     = "https://opencode.ai"
	opencodeChatURL     = "/zen/v1/chat/completions"
	opencodeMessagesURL = "/zen/v1/messages"
)

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
	ModelRegistrar       bool     `json:"model_registrar"`
	ModelProvider        bool     `json:"model_provider"`
	Executor             bool     `json:"executor"`
	ExecutorModelScope   string   `json:"executor_model_scope"`
	ExecutorInputFormats []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
}

// Executor RPC types
type executorRequest struct {
	AuthID          string            `json:"auth_id"`
	AuthProvider    string            `json:"auth_provider"`
	Model           string            `json:"model"`
	Format          string            `json:"format"`
	Stream          bool              `json:"stream"`
	Alt             string            `json:"alt,omitempty"`
	Headers         map[string][]string `json:"headers,omitempty"`
	SourceFormat    string            `json:"source_format"`
	Payload         json.RawMessage   `json:"payload"`
	OriginalRequest json.RawMessage   `json:"original_request"`
	StorageJSON     json.RawMessage   `json:"storage_json,omitempty"`
}

type executorStreamChunk struct {
	Payload []byte `json:"payload,omitempty"`
	Err     string `json:"error,omitempty"`
}

type executorStreamResponse struct {
	Headers map[string][]string    `json:"headers,omitempty"`
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
	Body       json.RawMessage     `json:"body,omitempty"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// requestBuffer holds the raw JSON from the last host call,
// so executor methods can decode it.
var (
	mu            sync.Mutex
	requestBuf    []byte
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

// Model definitions for OpenCode Free
var opencodeModels = []map[string]any{
	{"id": "claude-sonnet-4-7-20250507", "name": "claude-sonnet-4-7-20250507", "display_name": "Claude Sonnet 4.7", "object": "model", "owned_by": "anthropic", "type": "chat"},
	{"id": "claude-sonnet-4-6-20250507", "name": "claude-sonnet-4-6-20250507", "display_name": "Claude Sonnet 4.6", "object": "model", "owned_by": "anthropic", "type": "chat"},
	{"id": "claude-haiku-4-5-20250507", "name": "claude-haiku-4-5-20250507", "display_name": "Claude Haiku 4.5", "object": "model", "owned_by": "anthropic", "type": "chat"},
	{"id": "claude-opus-4-8-20250715", "name": "claude-opus-4-8-20250715", "display_name": "Claude Opus 4.8", "object": "model", "owned_by": "anthropic", "type": "chat"},
	{"id": "claude-fable-5-20250715", "name": "claude-fable-5-20250715", "display_name": "Claude Fable 5", "object": "model", "owned_by": "anthropic", "type": "chat"},
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
			Version:          "0.1.0",
			Author:           "nhymxu",
			GitHubRepository: "https://github.com/nhymxu/cpa-opencode-free",
			Logo:             "",
			ConfigFields:     nil,
		},
		Capabilities: capabilities{
			ModelRegistrar:       true,
			ModelProvider:        true,
			Executor:             true,
			ExecutorModelScope:   "both",
			ExecutorInputFormats: []string{"chat-completions"},
			ExecutorOutputFormats:  []string{"chat-completions"},
		},
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		return nil, err
	}
	return okEnvelopeRaw(raw), nil
}

func handleModelRegister() ([]byte, error) {
	modelsJSON, _ := json.Marshal(opencodeModels)
	return okEnvelopeJSON(`{"provider":"opencode-free","models":` + string(modelsJSON) + `}`)
}

func handleModelStatic() ([]byte, error) {
	modelsJSON, _ := json.Marshal(opencodeModels)
	return okEnvelopeJSON(`{"provider":"opencode-free","models":` + string(modelsJSON) + `}`)
}

func handleModelForAuth() ([]byte, error) {
	return okEnvelopeJSON(`{"handled":false}`)
}

func handleExecute() ([]byte, error) {
	req, payload, err := decodeExecutorRequest()
	if err != nil {
		return nil, fmt.Errorf("decode execute request: %w", err)
	}

	chatURL := selectChatURL(req.Format, req.SourceFormat)

	sendReq := hostHTTPRequest{
		Method: "POST",
		URL:    opencodeBaseURL + chatURL,
		Headers: map[string][]string{
			"Content-Type":      {"application/json"},
			"Authorization":     {"Bearer public"},
			"x-opencode-client": {"desktop"},
			"Accept":            {"application/json"},
		},
		Body: payload,
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

	sendReq := hostHTTPRequest{
		Method: "POST",
		URL:    opencodeBaseURL + chatURL,
		Headers: map[string][]string{
			"Content-Type":      {"application/json"},
			"Authorization":     {"Bearer public"},
			"x-opencode-client": {"desktop"},
			"Accept":            {"text/event-stream"},
		},
		Body: payload,
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
