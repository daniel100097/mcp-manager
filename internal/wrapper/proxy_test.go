package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type proxyResult struct {
	code int
	err  error
}
type proxyPeer struct {
	input    *io.PipeWriter
	output   *io.PipeReader
	messages chan json.RawMessage
	result   chan proxyResult
	cancel   context.CancelFunc
}

func startProxy(t *testing.T, serve func(context.Context, io.ReadCloser, io.Writer) (int, error)) *proxyPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	p := &proxyPeer{input: inputWriter, output: outputReader, messages: make(chan json.RawMessage, 32), result: make(chan proxyResult, 1), cancel: cancel}
	go func() {
		decoder := json.NewDecoder(outputReader)
		defer close(p.messages)
		for {
			var raw json.RawMessage
			if decoder.Decode(&raw) != nil {
				return
			}
			p.messages <- raw
		}
	}()
	go func() {
		code, err := serve(ctx, inputReader, outputWriter)
		outputWriter.Close()
		p.result <- proxyResult{code, err}
	}()
	t.Cleanup(func() { cancel(); inputWriter.Close(); outputReader.Close() })
	return p
}
func (p *proxyPeer) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, raw); err != nil {
		t.Fatal(err)
	}
}
func (p *proxyPeer) next(t *testing.T) json.RawMessage {
	t.Helper()
	select {
	case raw, ok := <-p.messages:
		if !ok {
			t.Fatal("proxy closed before the expected message")
		}
		return raw
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for proxy message")
		return nil
	}
}
func (p *proxyPeer) stop(t *testing.T, wantCode int) {
	t.Helper()
	p.input.Close()
	select {
	case result := <-p.result:
		if result.err != nil || result.code != wantCode {
			t.Fatalf("proxy exit = %d, %v; want %d", result.code, result.err, wantCode)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("proxy did not stop")
	}
}

func TestToolFilterPreservesMessagesAndFiltersEveryPage(t *testing.T) {
	f := newToolFilter([]string{"delete", "write"})
	list := `{"jsonrpc":"2.0","id":9007199254740993,"method":"tools/list","params":{"cursor":"page-2"}}`
	forward, local, err := f.outgoing([]byte(list))
	if err != nil || local != nil || string(forward) != list {
		t.Fatalf("outgoing: %s %s %v", forward, local, err)
	}
	input := `{"jsonrpc":"2.0","id":9007199254740993,"result":{"tools":[{"name":"delete","inputSchema":{}},{"name":"read","inputSchema":{"type":"object"},"future":true}],"nextCursor":"page-3","_meta":{"opaque":9}},"extension":"keep"}`
	result, err := f.incoming([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(result, []byte(`"delete"`)) || !bytes.Contains(result, []byte(`9007199254740993`)) || !bytes.Contains(result, []byte(`"nextCursor":"page-3"`)) || !bytes.Contains(result, []byte(`"future":true`)) || !bytes.Contains(result, []byte(`"extension":"keep"`)) {
		t.Fatalf("filtered result lost data: %s", result)
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`,
		`{"jsonrpc":"2.0","id":"server","method":"sampling/createMessage","params":{"messages":[]}}`,
	} {
		got, err := f.incoming([]byte(raw))
		if err != nil || string(got) != raw {
			t.Fatalf("forward %s: %s %v", raw, got, err)
		}
	}
	_, _, err = f.outgoing([]byte(`{"jsonrpc":"2.0","id":"\u0061","method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err = f.incoming([]byte(`{"jsonrpc":"2.0","id":"a","result":{"tools":[{"name":"delete"}],"nextCursor":"last"}}`))
	if err != nil || !bytes.Contains(result, []byte(`"tools":[]`)) {
		t.Fatalf("empty page: %s %v", result, err)
	}
	for _, id := range []string{`3`, `"call"`} {
		forward, local, err = f.outgoing([]byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"delete"}}`))
		if err != nil || forward != nil || !bytes.Contains(local, []byte(`"code":-32602`)) || !bytes.Contains(local, []byte(`"id":`+id)) {
			t.Fatalf("blocked call: %s %s %v", forward, local, err)
		}
	}
	forward, local, err = f.outgoing([]byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"write"}}`))
	if err != nil || forward != nil || local != nil {
		t.Fatalf("blocked notification: %s %s %v", forward, local, err)
	}
}

func TestProxySubprocess(t *testing.T) {
	if os.Getenv("MCP_MANAGER_PROXY_HELPER") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			os.Exit(7)
		}
		var msg wireMessage
		if json.Unmarshal(raw, &msg) != nil {
			os.Exit(12)
		}
		if msg.Method == "" {
			continue
		}
		if len(msg.ID) == 0 {
			encoder.Encode(raw)
			continue
		}
		result := any(map[string]any{"ok": true})
		switch msg.Method {
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read", "description": strings.Repeat("x", 100000), "inputSchema": map[string]any{"type": "object"}}, map[string]any{"name": "delete", "inputSchema": map[string]any{}}}, "nextCursor": "next"}
		case "tools/call":
			if bytes.Contains(msg.Params, []byte(`"delete"`)) {
				os.Exit(99)
			}
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": "from-server", "method": "roots/list"})
			var response wireMessage
			if decoder.Decode(&response) != nil || string(response.ID) != `"from-server"` {
				os.Exit(98)
			}
		}
		encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
}

func TestServeStdioFiltersAndRelaysBidirectionally(t *testing.T) {
	var stderr bytes.Buffer
	launch := Launch{Path: os.Args[0], Args: []string{os.Args[0], "-test.run=^TestProxySubprocess$"}, Env: append(os.Environ(), "MCP_MANAGER_PROXY_HELPER=1")}
	p := startProxy(t, func(ctx context.Context, input io.ReadCloser, output io.Writer) (int, error) {
		return ServeStdio(ctx, launch, []string{"delete"}, input, output, &stderr)
	})
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	raw := p.next(t)
	if bytes.Contains(raw, []byte(`"name":"delete"`)) || !bytes.Contains(raw, []byte(`"name":"read"`)) || len(raw) < 100000 {
		t.Fatalf("bad filtered list: length %d", len(raw))
	}
	p.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delete"}}`)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("disabled call succeeded: %s", raw)
	}
	p.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read"}}`)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`"method":"roots/list"`)) {
		t.Fatalf("missing server request: %s", raw)
	}
	p.send(t, `{"jsonrpc":"2.0","id":"from-server","result":{"roots":[]}}`)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`"ok":true`)) {
		t.Fatalf("missing call result: %s", raw)
	}
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`notifications/tools/list_changed`)) {
		t.Fatalf("notification lost: %s", raw)
	}
	p.stop(t, 7)
}

func TestToolFilterDropsReplayedResponses(t *testing.T) {
	f := newToolFilter([]string{"delete"})
	f.outgoing([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	response := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"delete"}]}}`)
	first, err := f.incoming(response)
	if err != nil || !bytes.Contains(first, []byte(`"tools":[]`)) {
		t.Fatalf("first response: %s %v", first, err)
	}
	if replay, err := f.incoming(response); err != nil || replay != nil {
		t.Fatalf("replay exposed a disabled tool: %s %v", replay, err)
	}
}

func TestRelayStopsOnInvalidServerMessage(t *testing.T) {
	transport := &fixtureTransport{messages: make(chan json.RawMessage, 1), closed: make(chan struct{})}
	transport.messages <- json.RawMessage(`not JSON`)
	input, writer := io.Pipe()
	defer writer.Close()
	err := relay(context.Background(), transport, []string{"delete"}, input, io.Discard)
	if err == nil {
		t.Fatal("accepted malformed server protocol")
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("transport was not closed")
	}
}

type fixtureTransport struct {
	messages chan json.RawMessage
	closed   chan struct{}
}

func (f *fixtureTransport) Read(ctx context.Context) (json.RawMessage, error) {
	select {
	case raw := <-f.messages:
		return raw, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fixtureTransport) Write(context.Context, json.RawMessage) error { return nil }
func (f *fixtureTransport) CloseInput()                                  {}
func (f *fixtureTransport) Close() error                                 { close(f.closed); return nil }

func TestRelayCancellationUnblocksClientOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, inputWriter := io.Pipe()
	defer inputWriter.Close()
	outputReader, output := io.Pipe()
	defer outputReader.Close()
	writer := &notifyingWriter{WriteCloser: output, started: make(chan struct{})}
	transport := &fixtureTransport{messages: make(chan json.RawMessage, 1), closed: make(chan struct{})}
	transport.messages <- json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	done := make(chan error, 1)
	go func() { done <- relay(ctx, transport, nil, input, writer) }()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release blocked stdout")
	}
}

type notifyingWriter struct {
	io.WriteCloser
	started chan struct{}
}

func (w *notifyingWriter) Write(data []byte) (int, error) {
	close(w.started)
	return w.WriteCloser.Write(data)
}
