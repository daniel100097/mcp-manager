package wrapper

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
)

// wireMessage keeps results and parameters opaque so extensions, structured
// content, pagination cursors, and numeric IDs survive the relay unchanged.
type wireMessage struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

type toolFilter struct {
	disabled map[string]bool
	mu       sync.Mutex
	pending  map[string]string
}

func newToolFilter(disabled []string) *toolFilter {
	f := &toolFilter{disabled: map[string]bool{}, pending: map[string]string{}}
	for _, name := range disabled {
		f.disabled[name] = true
	}
	return f
}

func requestKey(id json.RawMessage) (string, error) {
	var name string
	if json.Unmarshal(id, &name) == nil && string(id) != "null" {
		return "s:" + name, nil
	}
	if number, ok := new(big.Rat).SetString(string(id)); ok && number.IsInt() {
		return "n:" + number.String(), nil
	}
	return "", fmt.Errorf("JSON-RPC request ID must be a string or integer")
}

// outgoing returns a message for the server, or a local response when blocked.
func (f *toolFilter) outgoing(raw json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	var msg wireMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, nil, err
	}
	if msg.Method == "" {
		return raw, nil, nil
	} // response to a server request
	if msg.Method == "notifications/cancelled" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			if key, err := requestKey(params.RequestID); err == nil {
				f.mu.Lock()
				delete(f.pending, key)
				f.mu.Unlock()
			}
		}
	}
	if msg.Method == "tools/call" {
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, nil, fmt.Errorf("decode tools/call parameters: %w", err)
		}
		if f.disabled[params.Name] {
			if len(msg.ID) == 0 {
				return nil, nil, nil
			}
			response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32602, "message": fmt.Sprintf("Tool %q is disabled by mcp-manager", params.Name)}})
			return nil, response, err
		}
	}
	if len(msg.ID) > 0 {
		key, err := requestKey(msg.ID)
		if err != nil {
			return nil, nil, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, exists := f.pending[key]; exists {
			return nil, nil, fmt.Errorf("duplicate in-flight JSON-RPC request ID")
		}
		f.pending[key] = msg.Method
	}
	return raw, nil, nil
}

func (f *toolFilter) incoming(raw json.RawMessage) (json.RawMessage, error) {
	var msg wireMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	if msg.Method != "" || len(msg.ID) == 0 {
		return raw, nil
	} // server request or notification
	// Parse errors may have a null ID because no request could be identified.
	if string(msg.ID) == "null" && len(msg.Result) == 0 {
		return raw, nil
	}
	key, err := requestKey(msg.ID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	method, pending := f.pending[key]
	delete(f.pending, key)
	f.mu.Unlock()
	// SSE resumption may replay an already handled response. Never expose its
	// unfiltered result after the original request has been retired.
	if !pending {
		return nil, nil
	}
	if method != "tools/list" || len(msg.Result) == 0 || len(f.disabled) == 0 {
		return raw, nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		return nil, fmt.Errorf("decode tools/list result: %w", err)
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		return nil, fmt.Errorf("decode tools/list tools: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		var entry struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tool, &entry); err != nil {
			return nil, err
		}
		if !f.disabled[entry.Name] {
			kept = append(kept, tool)
		}
	}
	result["tools"], err = json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	envelope["result"], err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}
