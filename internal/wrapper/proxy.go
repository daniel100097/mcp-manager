package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type messageTransport interface {
	Read(context.Context) (json.RawMessage, error)
	Write(context.Context, json.RawMessage) error
	CloseInput()
	Close() error
}

// relay forwards both directions concurrently: a server may request sampling,
// roots or elicitation while a client call is still pending.
func relay(ctx context.Context, upstream messageTransport, disabled []string, stdin io.ReadCloser, stdout io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	filter := newToolFilter(disabled)
	var outputMu sync.Mutex
	write := func(raw json.RawMessage) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		return json.NewEncoder(stdout).Encode(raw)
	}
	clientDone, serverDone := make(chan error, 1), make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(stdin)
		for {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				clientDone <- err
				return
			}
			forward, response, err := filter.outgoing(raw)
			if err == nil && response != nil {
				err = write(response)
			}
			if err == nil && forward != nil {
				err = upstream.Write(ctx, forward)
			}
			if err != nil {
				clientDone <- err
				return
			}
		}
	}()
	go func() {
		for {
			raw, err := upstream.Read(ctx)
			if err == nil {
				raw, err = filter.incoming(raw)
			}
			if err == nil && raw != nil {
				err = write(raw)
			}
			if err != nil {
				serverDone <- err
				return
			}
		}
	}()
	var err error
	var clientFinished, serverFinished bool
	select {
	case err = <-clientDone:
		clientFinished = true
		if errors.Is(err, io.EOF) {
			upstream.CloseInput()
			// Let a well-behaved subprocess flush its final responses on stdin EOF;
			// bound shutdown for remote calls and servers that ignore EOF.
			select {
			case err = <-serverDone:
				serverFinished = true
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(2 * time.Second):
				err = nil
			}
		}
	case err = <-serverDone:
		serverFinished = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	cancel()
	// Own the input stream for the lifetime of this relay, so a stopped server
	// also releases a goroutine waiting for the agent's next message.
	_ = stdin.Close()
	// A client may stop reading while keeping its pipe open. Closing stdout
	// releases a blocked writer when this connection is cancelled or stops.
	if closer, ok := stdout.(io.Closer); ok {
		_ = closer.Close()
	}
	closeErr := upstream.Close()
	if !clientFinished {
		<-clientDone
	}
	if !serverFinished {
		<-serverDone
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("MCP relay: %w", err)
	}
	return closeErr
}
