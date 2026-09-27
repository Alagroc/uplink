// Package mcp implements the slice of the Model Context Protocol that uplink
// needs: a JSON-RPC 2.0 tool server over stdio and over streamable HTTP.
//
// Only tools are exposed. Resources and prompts are answered with empty lists
// so clients that probe for them do not report errors.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// LatestProtocol is the MCP revision uplink speaks. When a client asks for a
// different one we echo its choice back; the tool surface is identical across
// the revisions in use.
const LatestProtocol = "2025-06-18"

var knownProtocols = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// JSON-RPC error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Request is an incoming JSON-RPC 2.0 message. A message without an ID is a
// notification and gets no reply.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *Request) isNotification() bool { return len(r.ID) == 0 }

// Response is an outgoing JSON-RPC 2.0 message.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// Handler runs a tool. The returned string is sent back as text content.
// Returning an error marks the tool result as an error, which the calling model
// sees and can react to, rather than failing the whole request.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	Schema      Schema
	Handler     Handler
}

// Schema is a minimal JSON Schema object for a tool's arguments.
type Schema struct {
	Props    map[string]Prop
	Required []string
}

// Prop describes one argument.
type Prop struct {
	Type        string // string, integer, boolean, array
	Description string
	Enum        []string
	Items       string // element type when Type is "array"
	Default     any
}

// MarshalJSON renders the schema as the JSON Schema object MCP clients expect.
func (s Schema) MarshalJSON() ([]byte, error) {
	props := map[string]any{}
	for name, p := range s.Props {
		m := map[string]any{"type": p.Type}
		if p.Description != "" {
			m["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			m["enum"] = p.Enum
		}
		if p.Type == "array" {
			item := p.Items
			if item == "" {
				item = "string"
			}
			m["items"] = map[string]any{"type": item}
		}
		if p.Default != nil {
			m["default"] = p.Default
		}
		props[name] = m
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	} else {
		// Some clients reject a schema with no required key present.
		out["required"] = []string{}
	}
	return json.Marshal(out)
}

// Server is a set of tools reachable over stdio or HTTP.
type Server struct {
	Name         string
	Version      string
	Instructions string

	mu     sync.RWMutex
	order  []string
	byName map[string]*Tool
}

// NewServer returns an empty tool server.
func NewServer(name, version, instructions string) *Server {
	return &Server{Name: name, Version: version, Instructions: instructions, byName: map[string]*Tool{}}
}

// Add registers a tool, replacing any tool of the same name.
func (s *Server) Add(t Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byName[t.Name]; !dup {
		s.order = append(s.order, t.Name)
	}
	copied := t
	s.byName[t.Name] = &copied
}

// Tools lists the registered tools in registration order.
func (s *Server) Tools() []*Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Tool, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.byName[n])
	}
	return out
}

type toolDesc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema Schema `json:"inputSchema"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type initParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// Handle dispatches one request. It returns nil for notifications.
func (s *Server) Handle(ctx context.Context, req *Request) *Response {
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return s.fail(req, CodeInvalidRequest, "jsonrpc must be 2.0")
	}

	switch req.Method {
	case "initialize":
		var p initParams
		_ = json.Unmarshal(req.Params, &p)
		version := LatestProtocol
		if knownProtocols[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return s.ok(req, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		})

	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return nil

	case "ping":
		return s.ok(req, map[string]any{})

	case "tools/list":
		tools := s.Tools()
		descs := make([]toolDesc, 0, len(tools))
		for _, t := range tools {
			descs = append(descs, toolDesc{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
		}
		return s.ok(req, map[string]any{"tools": descs})

	case "resources/list":
		return s.ok(req, map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return s.ok(req, map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		return s.ok(req, map[string]any{"prompts": []any{}})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return s.fail(req, CodeInvalidParams, "bad params: "+err.Error())
		}
		s.mu.RLock()
		tool := s.byName[p.Name]
		s.mu.RUnlock()
		if tool == nil {
			return s.fail(req, CodeMethodNotFound, "no such tool: "+p.Name)
		}
		args := p.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		text, err := tool.Handler(ctx, args)
		if err != nil {
			// Surface tool failures as tool results, not transport errors: the
			// model can then read what went wrong and adapt.
			return s.ok(req, callResult{Content: []textContent{{Type: "text", Text: err.Error()}}, IsError: true})
		}
		return s.ok(req, callResult{Content: []textContent{{Type: "text", Text: text}}})
	}

	if req.isNotification() {
		return nil
	}
	return s.fail(req, CodeMethodNotFound, "unsupported method: "+req.Method)
}

func (s *Server) ok(req *Request, result any) *Response {
	if req.isNotification() {
		return nil
	}
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) fail(req *Request, code int, msg string) *Response {
	if req.isNotification() {
		return nil
	}
	return &Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: code, Message: msg}}
}

// decodeMessages accepts a single JSON-RPC message or a batch array.
func decodeMessages(raw json.RawMessage) (reqs []*Request, batch bool, err error) {
	trimmed := trimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, errors.New("empty message")
	}
	if trimmed[0] == '[' {
		var many []*Request
		if err := json.Unmarshal(trimmed, &many); err != nil {
			return nil, true, err
		}
		return many, true, nil
	}
	var one Request
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, false, err
	}
	return []*Request{&one}, false, nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
