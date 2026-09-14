package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

type processTransport struct {
	command    *exec.Cmd
	input      io.WriteCloser
	output     io.ReadCloser
	decoder    *json.Decoder
	closeInput sync.Once
	closeOnce  sync.Once
	err        error
}

func (p *processTransport) Read(context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := p.decoder.Decode(&raw)
	return raw, err
}
func (p *processTransport) Write(ctx context.Context, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.NewEncoder(p.input).Encode(raw)
}
func (p *processTransport) CloseInput() { p.closeInput.Do(func() { _ = p.input.Close() }) }
func (p *processTransport) Close() error {
	p.closeOnce.Do(func() {
		// A server can exit while leaving descendants in its process group.
		defer killProcess(p.command)
		p.CloseInput()
		// Read has either finished or the agent has disconnected. Close stdout to
		// release the reader before Wait closes the command's pipes.
		_ = p.output.Close()
		done := make(chan error, 1)
		go func() { done <- p.command.Wait() }()
		select {
		case p.err = <-done:
			return
		case <-time.After(time.Second):
		}
		terminateProcess(p.command)
		select {
		case p.err = <-done:
			return
		case <-time.After(time.Second):
		}
		killProcess(p.command)
		p.err = <-done
	})
	return p.err
}

// ServeStdio keeps the manager between the agent and the child process.
func ServeStdio(ctx context.Context, launch Launch, disabled []string, stdin io.ReadCloser, stdout, stderr io.Writer) (int, error) {
	command := exec.Command(launch.Path)
	command.Args, command.Env, command.Stderr = launch.Args, launch.Env, stderr
	configureProcess(command)
	input, err := command.StdinPipe()
	if err != nil {
		return 1, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return 1, err
	}
	if err := command.Start(); err != nil {
		input.Close()
		output.Close()
		return 1, err
	}
	upstream := &processTransport{command: command, input: input, output: output, decoder: json.NewDecoder(output)}
	err = relay(ctx, upstream, disabled, stdin, stdout)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if code := exitError.ExitCode(); code >= 0 {
			return code, nil
		}
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
