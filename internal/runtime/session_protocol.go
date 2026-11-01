package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

type sessionTransport struct {
	Input   io.WriteCloser
	Output  io.Reader
	Collect func(context.Context) ([]model.Artifact, error)
}

// executeSession runs the worker protocol over a provider's bidirectional process stream.
func executeSession(ctx context.Context, task engine.Task, session model.AgentSession, turn model.AgentTurn, controls <-chan engine.SessionControl, emit func(engine.SessionUpdate), transport sessionTransport, sink *logSink, secrets []string) (result engine.TurnResult, err error) {
	if emit == nil {
		emit = func(engine.SessionUpdate) {}
	}
	observer := &sessionObserver{threadID: session.ThreadID, emit: emit, redact: sink.redact, secrets: secrets, tails: map[string]string{}, completed: make(chan completedTurn, 1)}
	client := connectAppServer(transport.Input, transport.Output, observer.notification, func(stream, message string) { sink.write(stream, []byte(message+"\n")) })
	select {
	case <-client.ready:
	case <-client.done:
		return result, errors.New("session workspace setup or app-server startup failed; inspect execution logs")
	case <-ctx.Done():
		return result, ctx.Err()
	}
	call := func(method string, params, result any) error {
		callCtx, cancel := rpcContext(ctx)
		defer cancel()
		return client.call(callCtx, method, params, result)
	}
	if err = call("initialize", map[string]any{"clientInfo": map[string]string{"name": "cloudpilot", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return result, fmt.Errorf("initialize Codex: %w", err)
	}
	if err = client.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return result, err
	}
	threadParams := map[string]any{"cwd": "/workspace", "model": task.Worker.Model, "modelProvider": "cloudpilot", "approvalPolicy": "never", "sandbox": "danger-full-access"}
	method := "thread/start"
	if session.ThreadID != "" {
		method = "thread/resume"
		threadParams["threadId"] = session.ThreadID
		threadParams["excludeTurns"] = true
	} else {
		threadParams["ephemeral"] = false
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = call(method, threadParams, &started); err != nil {
		return result, fmt.Errorf("%s: %w", method, err)
	}
	if started.Thread.ID == "" {
		return result, errors.New("Codex returned an empty thread ID")
	}
	result.ThreadID = started.Thread.ID
	observer.setThread(result.ThreadID)
	emit(engine.SessionUpdate{ThreadID: result.ThreadID})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	text := turn.Input
	if session.ThreadID == "" {
		var formatted strings.Builder
		var event any
		if json.Unmarshal(task.Event.Payload, &event) == nil {
			body, _ := json.MarshalIndent(event, "", "  ")
			formatted.Write(body)
		} else {
			formatted.Write(task.Event.Payload)
		}
		text = "Feedback event JSON follows:\n" + formatted.String()
		if !turn.Initial && strings.TrimSpace(turn.Input) != "" {
			text += "\n\nAdditional instructions from the operator:\n" + turn.Input
		}
	}
	userInput := func(text string) []map[string]string { return []map[string]string{{"type": "text", "text": text}} }
	var startedTurn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err = call("turn/start", map[string]any{"threadId": result.ThreadID, "input": userInput(text), "clientUserMessageId": fmt.Sprintf("cloudpilot-turn-%d", turn.ID)}, &startedTurn); err != nil {
		return result, fmt.Errorf("start turn: %w", err)
	}
	result.ProviderTurnID = startedTurn.Turn.ID
	if result.ProviderTurnID == "" {
		return result, errors.New("Codex returned an empty turn ID")
	}
	observer.setTurn(result.ProviderTurnID)
	emit(engine.SessionUpdate{ProviderTurnID: result.ProviderTurnID})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	finish := func(completed completedTurn) (engine.TurnResult, error) {
		result.ProviderTurnID = completed.id
		result.Interrupted = completed.status == "interrupted"
		_ = transport.Input.Close()
		select {
		case <-client.done:
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(10 * time.Second):
			return result, errors.New("Codex did not stop after completing the turn")
		}
		collectionCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result.Artifacts, err = transport.Collect(collectionCtx)
		cancel()
		if err != nil {
			return result, fmt.Errorf("collect turn artifacts: %w", err)
		}
		if summary := observer.summary(); strings.TrimSpace(summary) != "" {
			result.Artifacts = append(result.Artifacts, model.Artifact{RunID: task.Run.ID, Kind: "text", Name: "Agent response", Content: summary, ContentType: "text/markdown"})
		}
		if completed.status == "failed" {
			if completed.message == "" {
				completed.message = "Codex turn failed"
			}
			return result, errors.New(completed.message)
		}
		return result, nil
	}
	for {
		select {
		case completed := <-observer.completed:
			return finish(completed)
		case control, ok := <-controls:
			if !ok {
				controls = nil
				continue
			}
			var controlErr error
			switch control.Type {
			case "message":
				controlErr = call("turn/steer", map[string]any{"threadId": result.ThreadID, "expectedTurnId": result.ProviderTurnID, "input": userInput(control.Text), "clientUserMessageId": fmt.Sprintf("cloudpilot-message-%d", control.ID)}, nil)
			case "interrupt":
				controlErr = call("turn/interrupt", map[string]string{"threadId": result.ThreadID, "turnId": result.ProviderTurnID}, nil)
			default:
				controlErr = errors.New("unsupported session control")
			}
			if control.ID != 0 {
				update := engine.SessionUpdate{ControlID: control.ID}
				if controlErr != nil {
					update.ControlError = sink.redact(controlErr.Error())
				}
				emit(update)
			}
			if controlErr != nil && control.Type == "interrupt" {
				emit(engine.SessionUpdate{ControlError: sink.redact(controlErr.Error())})
			}
		case <-ctx.Done():
			return result, ctx.Err()
		case <-client.done:
			select {
			case completed := <-observer.completed:
				return finish(completed)
			default:
			}
			return result, errors.New("Codex app-server exited before completing the turn")
		}
	}
}
