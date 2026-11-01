package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/BingyanStudio/CloudPilot/internal/sandbox/process"
	"github.com/BingyanStudio/CloudPilot/internal/sandbox/process/processconnect"
)

// Command describes one non-PTY process; stdout and stderr are separate byte streams.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Env    map[string]string
	User   string
	Tag    string
	Stdin  bool
	Stdout io.Writer
	Stderr io.Writer
}

// Process owns the command stream and implements an ordered stdin WriteCloser.
type Process struct {
	PID      uint32
	sandbox  *Sandbox
	rpc      processconnect.ProcessClient
	ctx      context.Context
	mu       sync.Mutex
	stdin    bool
	closed   bool
	done     chan struct{}
	exitCode int
	err      error
}

// SupportsStdinClose checks the upstream envd capability required by app-server.
func (s *Sandbox) SupportsStdinClose() bool {
	var major, minor, patch int
	if _, err := fmt.Sscanf(s.EnvdVersion, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false
	}
	return major > 0 || minor > 5 || minor == 5 && patch >= 2
}

func (s *Sandbox) processClient() processconnect.ProcessClient {
	return processconnect.NewProcessClient(s.client.httpClient(), s.envdURL, connect.WithProtoJSON())
}

func processRequest[T any](s *Sandbox, message *T) *connect.Request[T] {
	req := connect.NewRequest(message)
	s.headers(req.Header())
	return req
}

// Start launches a process and starts draining its output before returning its PID.
func (s *Sandbox) Start(ctx context.Context, command Command) (*Process, error) {
	if command.Path == "" {
		return nil, errors.New("sandbox command path is required")
	}
	if command.Stdout == nil {
		command.Stdout = io.Discard
	}
	if command.Stderr == nil {
		command.Stderr = io.Discard
	}
	rpc := s.processClient()
	request := processRequest(s, &pb.StartRequest{Process: &pb.ProcessConfig{Cmd: command.Path, Args: command.Args, Envs: command.Env, Cwd: &command.Dir}, Stdin: &command.Stdin})
	if command.Tag != "" {
		request.Msg.Tag = &command.Tag
	}
	if command.User != "" {
		request.Header().Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(command.User+":")))
	}
	request.Header().Set("Keepalive-Ping-Interval", "30")
	stream, err := rpc.Start(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("start sandbox command: %w", err)
	}
	if !stream.Receive() {
		stream.Close()
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("sandbox command stream ended before start")
	}
	start := stream.Msg().GetEvent().GetStart()
	if start == nil || start.Pid == 0 {
		stream.Close()
		return nil, errors.New("sandbox command did not return a PID")
	}
	p := &Process{PID: start.Pid, sandbox: s, rpc: rpc, ctx: ctx, stdin: command.Stdin, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer stream.Close()
		ended := false
		for stream.Receive() {
			event := stream.Msg().GetEvent()
			if data := event.GetData(); data != nil {
				var err error
				switch output := data.Output.(type) {
				case *pb.ProcessEvent_DataEvent_Stdout:
					_, err = command.Stdout.Write(output.Stdout)
				case *pb.ProcessEvent_DataEvent_Stderr:
					_, err = command.Stderr.Write(output.Stderr)
				}
				if err != nil {
					p.err = fmt.Errorf("consume sandbox output: %w", err)
					return
				}
			}
			if end := event.GetEnd(); end != nil {
				ended = true
				p.exitCode = int(end.ExitCode)
				if !end.Exited {
					p.err = errors.New("sandbox process ended without an exit status")
				}
				break
			}
		}
		if p.err == nil {
			p.err = stream.Err()
		}
		if !ended && p.err == nil {
			p.err = errors.New("sandbox command transport ended before process exit")
		}
	}()
	return p, nil
}

func (p *Process) selector() *pb.ProcessSelector {
	return &pb.ProcessSelector{Selector: &pb.ProcessSelector_Pid{Pid: p.PID}}
}

// Write sends stdin bytes in order; uncertain writes are never automatically replayed.
func (p *Process) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.stdin {
		return 0, io.ErrClosedPipe
	}
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	_, err := p.rpc.SendInput(ctx, processRequest(p.sandbox, &pb.SendInputRequest{Process: p.selector(), Input: &pb.ProcessInput{Input: &pb.ProcessInput_Stdin{Stdin: data}}}))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// Close signals EOF without cancelling the process output stream.
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if !p.sandbox.SupportsStdinClose() {
		return errors.New("sandbox envd does not support closing stdin; rebuild its template")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 10*time.Second)
	defer cancel()
	_, err := p.rpc.CloseStdin(ctx, processRequest(p.sandbox, &pb.CloseStdinRequest{Process: p.selector()}))
	if err == nil || connect.CodeOf(err) == connect.CodeNotFound {
		p.closed = true
		return nil
	}
	return err
}

// Wait returns only a confirmed process exit; a dropped stream is an error.
func (p *Process) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.exitCode, p.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Stop kills the remote command and verifies that its PID no longer exists.
func (p *Process) Stop(ctx context.Context) error {
	return p.sandbox.StopProcess(ctx, p.PID)
}

// StopProcess kills a previously recorded command and confirms its disappearance.
func (s *Sandbox) StopProcess(ctx context.Context, pid uint32) error {
	if pid == 0 {
		return errors.New("sandbox process ID must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rpc := s.processClient()
	selector := &pb.ProcessSelector{Selector: &pb.ProcessSelector_Pid{Pid: pid}}
	_, err := rpc.SendSignal(ctx, processRequest(s, &pb.SendSignalRequest{Process: selector, Signal: pb.Signal_SIGNAL_SIGKILL}))
	if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := rpc.List(ctx, processRequest(s, &pb.ListRequest{}))
		if err != nil {
			return err
		}
		found := false
		for _, process := range result.Msg.Processes {
			if process.Pid == pid {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
