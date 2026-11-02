package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

const archiveMCPBodyLimit = 64 << 10

type archiveRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type archiveRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type archiveRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *archiveRPCError `json:"error,omitempty"`
}

func (a *archiveStore) handler() http.Handler { return http.HandlerFunc(a.serveMCP) }

func archiveMCPVersion(v string) bool {
	switch v {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return true
	}
	return false
}

func archiveRPCWrite(w http.ResponseWriter, id json.RawMessage, result any, code int, message string, status int) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	response := archiveRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
	if code != 0 {
		response.Error = &archiveRPCError{Code: code, Message: message}
		response.Result = nil
	}
	data, err := json.Marshal(response)
	if err != nil || len(data) > 2<<20 {
		data, _ = json.Marshal(archiveRPCResponse{JSONRPC: "2.0", ID: id, Error: &archiveRPCError{Code: -32603, Message: "response unavailable"}})
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func archiveRPCID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(id))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	}
	return false
}

func (a *archiveStore) serveMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// CLI endpoint: browsers do not need access, even from the same origin.
	if len(r.Header.Values("Origin")) != 0 {
		http.Error(w, "browser origins are not permitted", http.StatusForbidden)
		return
	}
	auth := r.Header.Values("Authorization")
	var token string
	if len(auth) == 1 {
		parts := strings.Fields(auth[0])
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token = parts[1]
		}
	}
	namespace, ok := a.tokenNamespace(token)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="token-trim"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	accept := r.Header.Get("Accept")
	if accept != "" && !strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*") {
		http.Error(w, "application/json response required", http.StatusNotAcceptable)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !archiveMCPVersion(version) {
		http.Error(w, "unsupported protocol version", http.StatusBadRequest)
		return
	}
	body := http.MaxBytesReader(w, r.Body, archiveMCPBodyLimit)
	data, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "request too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var req archiveRPCRequest
	if !json.Valid(data) {
		archiveRPCWrite(w, nil, nil, -32700, "parse error", http.StatusBadRequest)
		return
	}
	if json.Unmarshal(data, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" || !archiveRPCID(req.ID) {
		archiveRPCWrite(w, nil, nil, -32600, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		// Notifications are one-way, and must never trigger tool execution.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := a.archiveRPC(namespace, req)
	if rpcErr != nil {
		archiveRPCWrite(w, req.ID, nil, rpcErr.Code, rpcErr.Message, http.StatusOK)
		return
	}
	archiveRPCWrite(w, req.ID, result, 0, "", http.StatusOK)
}

func archiveDecodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func (a *archiveStore) archiveRPC(namespace string, req archiveRPCRequest) (any, *archiveRPCError) {
	invalid := func() (any, *archiveRPCError) {
		return nil, &archiveRPCError{Code: -32602, Message: "invalid parameters"}
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
			return invalid()
		}
		version := params.ProtocolVersion
		if !archiveMCPVersion(version) {
			version = "2025-11-25"
		}
		return map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "token-trim-archive", "version": "1.0.0"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var params struct {
			Cursor string          `json:"cursor"`
			Meta   json.RawMessage `json:"_meta"`
		}
		if archiveDecodeParams(req.Params, &params) != nil || params.Cursor != "" {
			return invalid()
		}
		return map[string]any{"tools": archiveMCPTools()}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta"`
		}
		if archiveDecodeParams(req.Params, &params) != nil {
			return invalid()
		}
		var value any
		var err error
		switch params.Name {
		case "token_trim_read":
			var args struct {
				ID     string `json:"id"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if archiveDecodeParams(params.Arguments, &args) != nil {
				err = errArchiveInput
			} else {
				value, err = a.read(namespace, args.ID, args.Offset, args.Limit)
			}
		case "token_trim_search":
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if archiveDecodeParams(params.Arguments, &args) != nil {
				err = errArchiveInput
			} else {
				value, err = a.search(namespace, args.Query, args.Limit)
			}
		default:
			return nil, &archiveRPCError{Code: -32602, Message: "unknown tool"}
		}
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": archiveToolError(err)}}, "isError": true}, nil
		}
		structured := map[string]any{"data": value, "provenance": map[string]any{"source": "historical_archive", "current_disk_state_verified": false, "editing_requires_current_file_check": true, "notice": "Historical snapshot; current files on disk may differ."}}
		encoded, err := json.Marshal(structured)
		if err != nil {
			return nil, &archiveRPCError{Code: -32603, Message: "response unavailable"}
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}, "structuredContent": structured, "isError": false}, nil
	default:
		return nil, &archiveRPCError{Code: -32601, Message: "method not found"}
	}
}

func archiveMCPTools() []any {
	annotations := map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	return []any{
		map[string]any{"name": "token_trim_read", "description": "Read an archived historical document by ID, in UTF-8-safe byte pages. Use next_offset for the following page. Archive data is not current disk state.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}, "offset": map[string]any{"type": "integer", "minimum": 0, "default": 0}, "limit": map[string]any{"type": "integer", "minimum": 4, "maximum": archiveReadLimit, "default": 16384}}, "required": []string{"id"}, "additionalProperties": false}, "annotations": annotations},
		map[string]any{"name": "token_trim_search", "description": "Search historical archived documents in this token's workspace using case-insensitive keywords. Results are deterministic; truncated indicates the bounded scan or result limit was reached.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 10}}, "required": []string{"query"}, "additionalProperties": false}, "annotations": annotations},
	}
}
