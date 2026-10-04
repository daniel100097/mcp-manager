package wrapper

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/daniel100097/mcp-manager/internal/config"
)

type httpMessage struct {
	raw json.RawMessage
	err error
}

type httpTransport struct {
	endpoint                                      string
	headers                                       http.Header
	client                                        *http.Client
	ctx                                           context.Context
	cancel                                        context.CancelFunc
	incoming                                      chan httpMessage
	inputDone                                     chan struct{}
	mu                                            sync.Mutex
	session, version, initializeID                string
	inputClosed, inputFinished, closed, listening bool
	calls                                         int
	workers                                       sync.WaitGroup
	closeOnce                                     sync.Once
}

// ServeHTTP presents a streamable HTTP or legacy HTTP+SSE MCP as stdio to every
// supported agent. The agent's initialization, capabilities, IDs and
// notifications pass through.
func ServeHTTP(ctx context.Context, mcp config.MCP, environ, disabled []string, stdin io.ReadCloser, stdout io.Writer) (int, error) {
	transport, err := newHTTPTransport(ctx, mcp, environ)
	if err != nil {
		return 1, err
	}
	var upstream messageTransport = transport
	if mcp.Type == "sse" {
		if upstream, err = transport.connectSSE(); err != nil {
			return 1, err
		}
	}
	if err := relay(ctx, upstream, disabled, stdin, stdout); err != nil {
		return 1, err
	}
	return 0, nil
}

func newHTTPTransport(ctx context.Context, mcp config.MCP, environ []string) (*httpTransport, error) {
	endpoint := mcp.URL
	if mcp.URLFrom != "" {
		value, ok := lookupEnv(environ, mcp.URLFrom)
		if !ok {
			return nil, fmt.Errorf("HTTP MCP requires environment variable %q, which is not set", mcp.URLFrom)
		}
		if err := config.ValidateURL(value); err != nil {
			return nil, fmt.Errorf("HTTP MCP URL from %q: %w", mcp.URLFrom, err)
		}
		endpoint = value
	}
	headers := make(http.Header)
	for name, value := range mcp.Headers {
		headers.Set(name, value)
	}
	for name, variable := range mcp.HeadersFrom {
		value, ok := lookupEnv(environ, variable)
		if !ok {
			return nil, fmt.Errorf("HTTP MCP requires environment variable %q, which is not set", variable)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("HTTP header %q from %q contains an invalid character", name, variable)
		}
		headers.Set(name, value)
	}
	ctx, cancel := context.WithCancel(ctx)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &httpTransport{
		endpoint: endpoint, headers: headers, ctx: ctx, cancel: cancel,
		incoming: make(chan httpMessage, 32), inputDone: make(chan struct{}),
		client: &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Header credentials belong to the configured origin, including custom
			// headers which net/http would otherwise forward across origins.
			if len(via) >= 10 {
				return errors.New("too many HTTP redirects")
			}
			first := via[0].URL
			if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
				return errors.New("HTTP MCP redirect changes origin")
			}
			return nil
		}},
	}, nil
}

func (h *httpTransport) Read(ctx context.Context) (json.RawMessage, error) {
	// Drain responses queued before the final HTTP call finished.
	select {
	case event := <-h.incoming:
		return event.raw, event.err
	default:
	}
	select {
	case event := <-h.incoming:
		return event.raw, event.err
	case <-h.inputDone:
		select {
		case event := <-h.incoming:
			return event.raw, event.err
		default:
			return nil, io.EOF
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.ctx.Done():
		return nil, h.ctx.Err()
	}
}

func (h *httpTransport) Write(ctx context.Context, raw json.RawMessage) error {
	var msg wireMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	isCall := msg.Method != "" && len(msg.ID) > 0
	key := ""
	if isCall {
		var err error
		key, err = requestKey(msg.ID)
		if err != nil {
			return err
		}
	}
	h.mu.Lock()
	if h.closed || h.inputClosed {
		h.mu.Unlock()
		return io.ErrClosedPipe
	}
	if msg.Method == "initialize" {
		h.initializeID = key
	}
	h.workers.Add(1)
	if isCall {
		h.calls++
	}
	h.mu.Unlock()
	send := func() error {
		requestContext, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		defer cancel()
		if err := h.post(requestContext, raw, key); err != nil {
			return err
		}
		if msg.Method == "notifications/initialized" {
			h.startListening()
		}
		return nil
	}
	if isCall {
		// A slow request must not prevent cancellation notifications or responses
		// to server-initiated requests from being sent on another HTTP request.
		go func() {
			defer h.workers.Done()
			defer h.finishCall()
			if err := send(); err != nil {
				h.deliver(nil, err)
			}
		}()
		return nil
	}
	defer h.workers.Done()
	return send()
}

func (h *httpTransport) finishCall() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls--
	h.finishInputLocked()
}
func (h *httpTransport) finishInputLocked() {
	if h.inputClosed && h.calls == 0 && !h.inputFinished {
		h.inputFinished = true
		close(h.inputDone)
	}
}
func (h *httpTransport) CloseInput() {
	h.mu.Lock()
	h.inputClosed = true
	h.finishInputLocked()
	h.mu.Unlock()
}

func (h *httpTransport) request(ctx context.Context, method string, body io.Reader, lastID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header = h.headers.Clone()
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		req.Header.Set("Accept", "text/event-stream")
	}
	h.mu.Lock()
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	if h.version != "" {
		req.Header.Set("MCP-Protocol-Version", h.version)
	}
	h.mu.Unlock()
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	return h.client.Do(req)
}

func (h *httpTransport) post(ctx context.Context, raw json.RawMessage, key string) error {
	resp, err := h.request(ctx, http.MethodPost, bytes.NewReader(raw), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP MCP POST returned %s", resp.Status)
	}
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		h.mu.Lock()
		previous := h.session
		if previous == "" {
			h.session = session
		}
		h.mu.Unlock()
		if previous != "" && previous != session {
			return errors.New("HTTP MCP changed its session ID")
		}
	}
	if key == "" {
		return nil
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch contentType {
	case "application/json":
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return fmt.Errorf("decode HTTP MCP response: %w", err)
		}
		complete, err := h.forward(raw, key)
		if err == nil && !complete {
			return errors.New("HTTP MCP JSON response did not match its request ID")
		}
		return err
	case "text/event-stream":
		return h.listen(ctx, resp, key)
	default:
		return fmt.Errorf("HTTP MCP response has unsupported Content-Type %q", contentType)
	}
}

func (h *httpTransport) deliver(raw json.RawMessage, err error) {
	select {
	case h.incoming <- httpMessage{raw, err}:
	case <-h.ctx.Done():
	}
}

func (h *httpTransport) forward(raw json.RawMessage, wanted string) (bool, error) {
	var msg wireMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false, fmt.Errorf("decode HTTP MCP message: %w", err)
	}
	key := ""
	if msg.Method == "" && len(msg.ID) > 0 {
		var err error
		key, err = requestKey(msg.ID)
		if err != nil {
			return false, err
		}
	}
	h.mu.Lock()
	if key != "" && key == h.initializeID && len(msg.Result) > 0 {
		var result struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(msg.Result, &result); err != nil || result.ProtocolVersion == "" {
			h.mu.Unlock()
			return false, errors.New("HTTP MCP returned an invalid initialize result")
		}
		h.version = result.ProtocolVersion
	}
	h.mu.Unlock()
	h.deliver(raw, nil)
	return key != "" && key == wanted, nil
}

func (h *httpTransport) startListening() {
	h.mu.Lock()
	if h.closed || h.listening {
		h.mu.Unlock()
		return
	}
	h.listening = true
	h.workers.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.workers.Done()
		if err := h.listen(h.ctx, nil, ""); err != nil && h.ctx.Err() == nil {
			h.deliver(nil, err)
		}
	}()
}

// listen handles the optional standalone stream and resumption of interrupted
// POST streams. Calls are never retried with POST (which could repeat effects).
func (h *httpTransport) listen(ctx context.Context, resp *http.Response, key string) error {
	lastID := ""
	delay := 200 * time.Millisecond
	for attempt := 0; ; attempt++ {
		if resp == nil {
			var err error
			resp, err = h.request(ctx, http.MethodGet, nil, lastID)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if attempt >= 5 {
					return fmt.Errorf("reconnect HTTP MCP stream: %w", err)
				}
				if err := waitStream(ctx, delay); err != nil {
					return err
				}
				continue
			}
			if resp.StatusCode == http.StatusMethodNotAllowed && key == "" {
				resp.Body.Close()
				return nil
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return fmt.Errorf("HTTP MCP GET returned %s", resp.Status)
			}
			contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			if contentType != "text/event-stream" {
				resp.Body.Close()
				return errors.New("HTTP MCP GET did not return an event stream")
			}
		}
		previousID := lastID
		complete, err := readEventStream(resp.Body, &lastID, &delay, func(event string, raw json.RawMessage) (bool, error) {
			if event != "message" {
				return false, nil
			}
			return h.forward(raw, key)
		})
		resp.Body.Close()
		resp = nil
		if complete {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var invalid *invalidEventError
		if errors.As(err, &invalid) {
			return err
		}
		if key != "" && lastID == "" {
			return errors.New("HTTP MCP response stream ended before its response, without an event ID to resume")
		}
		if previousID != lastID {
			attempt = 0
		}
		if attempt >= 5 {
			return errors.New("HTTP MCP event stream repeatedly disconnected")
		}
		if err := waitStream(ctx, delay); err != nil {
			return err
		}
	}
}

func waitStream(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *httpTransport) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		h.cancel()
		h.workers.Wait()
		h.mu.Lock()
		session := h.session
		h.mu.Unlock()
		if session != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if resp, err := h.request(ctx, http.MethodDelete, nil, ""); err == nil {
				resp.Body.Close()
			}
		}
		h.client.CloseIdleConnections()
	})
	return nil
}

// sseTransport speaks the legacy HTTP+SSE transport: one GET event stream
// carries every server message, and its endpoint event names the URL that
// receives each client message as a POST.
type sseTransport struct{ *httpTransport }

func (h *httpTransport) connectSSE() (*sseTransport, error) {
	resp, err := h.request(h.ctx, http.MethodGet, nil, "")
	if err != nil {
		h.Close()
		return nil, err
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || contentType != "text/event-stream" {
		resp.Body.Close()
		h.Close()
		return nil, fmt.Errorf("SSE MCP GET returned %s without an event stream", resp.Status)
	}
	ready := make(chan error, 1)
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		defer resp.Body.Close()
		var lastID string
		var retry time.Duration
		connected := false
		_, err := readEventStream(resp.Body, &lastID, &retry, func(event string, data json.RawMessage) (bool, error) {
			switch {
			case event == "endpoint" && !connected:
				// Header credentials must not leave the server's origin.
				base := resp.Request.URL
				target, err := base.Parse(strings.TrimSpace(string(data)))
				if err != nil || target.Scheme != base.Scheme || target.Host != base.Host {
					return false, errors.New("endpoint must share the server's origin")
				}
				h.endpoint, connected = target.String(), true
				ready <- nil
			case event == "message" && connected:
				h.deliver(data, nil)
			}
			return false, nil
		})
		if !connected {
			ready <- fmt.Errorf("connect SSE MCP: %w", err)
			return
		}
		h.deliver(nil, err)
	}()
	if err := <-ready; err != nil {
		h.Close()
		return nil, err
	}
	return &sseTransport{h}, nil
}

// Write posts a client message to the announced endpoint. Replies arrive on
// the event stream, so the POST response carries nothing to forward.
func (s *sseTransport) Write(ctx context.Context, raw json.RawMessage) error {
	resp, err := s.request(ctx, http.MethodPost, bytes.NewReader(raw), "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("SSE MCP POST returned %s", resp.Status)
	}
	return nil
}

// CloseInput keeps the stream open: replies to pending calls still arrive on
// it. ponytail: stdin EOF therefore waits out the relay's 2s grace; count
// pending calls if a faster exit matters (agents normally signal instead).
func (s *sseTransport) CloseInput() {}

type invalidEventError struct{ err error }

func (e *invalidEventError) Error() string { return "invalid HTTP MCP event: " + e.err.Error() }
func (e *invalidEventError) Unwrap() error { return e.err }

// SSE data lines may span multiple lines and exceed bufio.Scanner's 64 KiB
// default. Comments, LF/CRLF/CR, event IDs, and retry hints are supported.
// emit receives each event's type, "message" when unnamed, and its data.
func readEventStream(reader io.Reader, lastID *string, retry *time.Duration, emit func(string, json.RawMessage) (bool, error)) (bool, error) {
	input := bufio.NewReader(reader)
	var data []string
	event := ""
	pendingID := *lastID
	first := true
	skipLF := false
	readLine := func() (string, error) {
		var line strings.Builder
		for {
			b, err := input.ReadByte()
			if err != nil {
				return "", err
			}
			if skipLF {
				skipLF = false
				if b == '\n' {
					continue
				}
			}
			if b == '\r' {
				skipLF = true
				return line.String(), nil
			}
			if b == '\n' {
				return line.String(), nil
			}
			line.WriteByte(b)
		}
	}
	for {
		line, err := readLine()
		if err != nil {
			return false, err
		}
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			*lastID = pendingID
			if len(data) > 0 {
				done, err := emit(cmp.Or(event, "message"), json.RawMessage(strings.Join(data, "\n")))
				if err != nil {
					return false, &invalidEventError{err}
				}
				if done {
					return true, nil
				}
			}
			data = nil
			event = ""
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
		case "event":
			event = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				pendingID = value
			}
		case "retry":
			if duration, err := time.ParseDuration(value + "ms"); err == nil && duration >= 0 {
				*retry = max(10*time.Millisecond, min(duration, 30*time.Second))
			}
		}
	}
}
