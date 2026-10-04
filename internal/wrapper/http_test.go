package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daniel100097/mcp-manager/internal/config"
)

func TestServeHTTPJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			var calls atomic.Int32
			deleted := make(chan struct{}, 1)
			serverReply := make(chan struct{}, 1)
			events := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Region") != "eu" {
					t.Error("missing HTTP credentials")
				}
				if r.Method == http.MethodDelete {
					deleted <- struct{}{}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodGet {
					if r.Header.Get("Mcp-Session-Id") != "session-one" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
						t.Error("GET missing session or protocol")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					for {
						select {
						case event := <-events:
							fmt.Fprintf(w, "data: %s\n\n", event)
							w.(http.Flusher).Flush()
						case <-r.Context().Done():
							return
						}
					}
				}
				var msg wireMessage
				if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if msg.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "session-one" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18") {
					t.Error("POST missing session or negotiated protocol")
				}
				if msg.Method == "" {
					serverReply <- struct{}{}
					w.WriteHeader(202)
					return
				}
				if len(msg.ID) == 0 {
					w.WriteHeader(202)
					return
				}
				var result any
				switch msg.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "session-one")
					result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
				case "tools/list":
					result = map[string]any{"tools": []any{map[string]any{"name": "read", "inputSchema": map[string]any{}}, map[string]any{"name": "delete", "inputSchema": map[string]any{}}}, "nextCursor": "more"}
				case "tools/call":
					calls.Add(1)
					if bytes.Contains(msg.Params, []byte("delete")) {
						t.Error("disabled tool reached HTTP server")
					}
					events <- `{"jsonrpc":"2.0","id":"server-id","method":"roots/list"}`
					select {
					case <-serverReply:
					case <-r.Context().Done():
						return
					}
					result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "structuredContent": map[string]any{"answer": 42}, "isError": false}
				}
				raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
					fmt.Fprint(w, ": heartbeat\r\nid: 1\r\nevent: message\r\ndata: ")
					w.Write(raw)
					fmt.Fprint(w, "\r\n\r\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(raw)
				}
			}))
			defer server.Close()
			mcp := config.MCP{Type: "http", URL: server.URL, Headers: map[string]string{"X-Region": "eu"}, HeadersFrom: map[string]string{"Authorization": "TOKEN"}}
			p := startProxy(t, func(ctx context.Context, input io.ReadCloser, output io.Writer) (int, error) {
				return ServeHTTP(ctx, mcp, []string{"TOKEN=Bearer test-token"}, []string{"delete"}, input, output)
			})
			p.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"roots":{}},"clientInfo":{"name":"fixture","version":"1"}}}`)
			if raw := p.next(t); !bytes.Contains(raw, []byte(`"protocolVersion":"2025-06-18"`)) {
				t.Fatalf("initialize: %s", raw)
			}
			p.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			for _, id := range []int{2, 3} {
				p.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{"cursor":"page"}}`, id))
				raw := p.next(t)
				if bytes.Contains(raw, []byte(`"delete"`)) || !bytes.Contains(raw, []byte(`"read"`)) || !bytes.Contains(raw, []byte(`"nextCursor":"more"`)) {
					t.Fatalf("list: %s", raw)
				}
			}
			p.send(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete"}}`)
			if raw := p.next(t); !bytes.Contains(raw, []byte(`"error"`)) {
				t.Fatalf("disabled call: %s", raw)
			}
			p.send(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read"}}`)
			if raw := p.next(t); !bytes.Contains(raw, []byte(`"method":"roots/list"`)) {
				t.Fatalf("server request: %s", raw)
			}
			p.send(t, `{"jsonrpc":"2.0","id":"server-id","result":{"roots":[]}}`)
			if raw := p.next(t); !bytes.Contains(raw, []byte(`"structuredContent":{"answer":42}`)) {
				t.Fatalf("call result: %s", raw)
			}
			events <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
			if raw := p.next(t); !bytes.Contains(raw, []byte(`notifications/tools/list_changed`)) {
				t.Fatalf("notification: %s", raw)
			}
			p.stop(t, 0)
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d", calls.Load())
			}
			select {
			case <-deleted:
			case <-time.After(time.Second):
				t.Fatal("HTTP session was not deleted")
			}
		})
	}
}

func TestHTTPResumesPostSSEWithoutRepeatingCall(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if r.Method == http.MethodPost {
			posts.Add(1)
			fmt.Fprint(w, "id: resume-point\nretry: 10\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"waiting\"}}\n\n")
		} else {
			if r.Header.Get("Last-Event-ID") != "resume-point" {
				t.Error("missing resume cursor")
			}
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"delete\"}]}}\n\n")
		}
	}))
	defer server.Close()
	p := startProxy(t, func(ctx context.Context, input io.ReadCloser, output io.Writer) (int, error) {
		return ServeHTTP(ctx, config.MCP{Type: "http", URL: server.URL}, nil, []string{"delete"}, input, output)
	})
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	p.next(t)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`"tools":[]`)) {
		t.Fatalf("resumed result: %s", raw)
	}
	p.stop(t, 0)
	if posts.Load() != 1 {
		t.Fatal("POST was replayed")
	}
}

func TestHTTPMissingCredentialsFailBeforeConnecting(t *testing.T) {
	_, err := newHTTPTransport(context.Background(), config.MCP{Type: "http", URL: "https://example.test/mcp", HeadersFrom: map[string]string{"Authorization": "MISSING"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing credential: %v", err)
	}
}

func TestSSESupportsLargeMultilineDataAndComments(t *testing.T) {
	payload := strings.Repeat("x", 100000)
	input := ": keepalive\r\nid: abc\r\nretry: 25\r\nevent: message\r\ndata: {\"value\":\"" + payload + "\",\r\ndata: \"ok\":true}\r\n\r\n"
	var id string
	var retry time.Duration
	done, err := readEventStream(strings.NewReader(input), &id, &retry, func(_ string, raw json.RawMessage) (bool, error) {
		var result struct {
			Value string
			OK    bool
		}
		err := json.Unmarshal(raw, &result)
		if result.Value != payload || !result.OK {
			t.Error("SSE data was corrupted")
		}
		return true, err
	})
	if err != nil || !done || id != "abc" || retry != 25*time.Millisecond {
		t.Fatalf("SSE: %v %v %q %v", done, err, id, retry)
	}
}

func TestHTTPWithoutStandaloneSSEAndJSONRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(405)
			return
		}
		var msg wireMessage
		json.NewDecoder(r.Body).Decode(&msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "upstream error", "data": map[string]any{"detail": "keep"}}})
	}))
	defer server.Close()
	p := startProxy(t, func(ctx context.Context, input io.ReadCloser, output io.Writer) (int, error) {
		return ServeHTTP(ctx, config.MCP{Type: "http", URL: server.URL}, nil, nil, input, output)
	})
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"unknown/method"}`)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`"detail":"keep"`)) {
		t.Fatalf("error not preserved: %s", raw)
	}
	p.stop(t, 0)
}

func TestHTTPCancellationCanInterruptPendingCall(t *testing.T) {
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg wireMessage
		json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == "notifications/cancelled" {
			close(cancelled)
			w.WriteHeader(202)
			return
		}
		select {
		case <-cancelled:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"cancelled"}}`)
	}))
	defer server.Close()
	p := startProxy(t, func(ctx context.Context, input io.ReadCloser, output io.Writer) (int, error) {
		return ServeHTTP(ctx, config.MCP{Type: "http", URL: server.URL}, nil, nil, input, output)
	})
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`)
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked behind call")
	}
	p.stop(t, 0)
}

func TestHTTPShutdownCancelsBlockedRequests(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	upstream, err := newHTTPTransport(context.Background(), config.MCP{Type: "http", URL: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := upstream.Write(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	done := make(chan struct{})
	go func() { upstream.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on HTTP response")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request was not cancelled")
	}
}

func TestHTTPFailuresAndRedirectsDoNotLeakCredentials(t *testing.T) {
	for _, status := range []int{401, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, "secret from upstream")
			}))
			defer server.Close()
			input := io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			code, err := ServeHTTP(context.Background(), config.MCP{Type: "http", URL: server.URL}, nil, nil, input, io.Discard)
			if code == 0 || err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) || strings.Contains(err.Error(), "secret from upstream") {
				t.Fatalf("HTTP status %d = %d %v", status, code, err)
			}
		})
	}
	var reached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	input := io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	_, err := ServeHTTP(context.Background(), config.MCP{Type: "http", URL: source.URL, Headers: map[string]string{"X-API-Key": "credential"}}, nil, nil, input, io.Discard)
	if err == nil || reached.Load() {
		t.Fatalf("cross-origin redirect was followed: %v", err)
	}
	announcer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: endpoint\ndata: %s/messages\n\n", destination.URL)
	}))
	defer announcer.Close()
	input = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	_, err = ServeHTTP(context.Background(), config.MCP{Type: "sse", URL: announcer.URL, Headers: map[string]string{"X-API-Key": "credential"}}, nil, nil, input, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "origin") || reached.Load() {
		t.Fatalf("cross-origin SSE endpoint was used: %v", err)
	}
}

func TestServeSSEFromURLVariable(t *testing.T) {
	events := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing SSE credentials")
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /messages?session=one\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case event := <-events:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", event)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		if r.URL.Path != "/messages" || r.URL.Query().Get("session") != "one" {
			t.Errorf("POST to %s, want the announced endpoint", r.URL)
		}
		var msg wireMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusAccepted)
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read", "inputSchema": map[string]any{}}, map[string]any{"name": "delete", "inputSchema": map[string]any{}}}}
		default:
			return
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		events <- string(raw)
	}))
	defer server.Close()
	mcp := config.MCP{Type: "sse", URLFrom: "EVENTS_URL", HeadersFrom: map[string]string{"Authorization": "TOKEN"}}
	environ := []string{"EVENTS_URL=" + server.URL + "/sse", "TOKEN=Bearer test-token"}
	p := startProxy(t, func(ctx context.Context, input io.ReadCloser, output io.Writer) (int, error) {
		return ServeHTTP(ctx, mcp, environ, []string{"delete"}, input, output)
	})
	p.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`)
	if raw := p.next(t); !bytes.Contains(raw, []byte(`"protocolVersion":"2024-11-05"`)) {
		t.Fatalf("initialize: %s", raw)
	}
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	p.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if raw := p.next(t); bytes.Contains(raw, []byte(`"delete"`)) || !bytes.Contains(raw, []byte(`"read"`)) {
		t.Fatalf("list: %s", raw)
	}
	p.stop(t, 0)
}

func TestSSECursorOnlyCommitsCompleteEvents(t *testing.T) {
	id := "previous"
	var retry time.Duration
	_, err := readEventStream(strings.NewReader("id: undelivered\ndata: {"), &id, &retry, func(string, json.RawMessage) (bool, error) { t.Fatal("incomplete event delivered"); return false, nil })
	if err == nil || id != "previous" {
		t.Fatalf("incomplete event advanced cursor: %q %v", id, err)
	}
}

func TestSSELineEndings(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			var id string
			var retry time.Duration
			done, err := readEventStream(strings.NewReader("data: {}"+ending+ending), &id, &retry, func(string, json.RawMessage) (bool, error) { return true, nil })
			if !done || err != nil {
				t.Fatalf("line ending %q: %v %v", ending, done, err)
			}
		})
	}
}
