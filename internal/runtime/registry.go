package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// Runners routes execution to registered compute providers. Configure it before starting the engine.
type Runners map[string]engine.Executor

func (r Runners) Execute(ctx context.Context, task engine.Task, log func(string, string)) ([]model.Artifact, error) {
	executor, ok := r[task.Runner.Type]
	if !ok || executor == nil {
		return nil, fmt.Errorf("runner %q is not registered", task.Runner.Type)
	}
	return executor.Execute(ctx, task, log)
}

// Recover reconciles provider state before interrupted events are eligible for another attempt.
func (r Runners) Recover(ctx context.Context) error {
	var failures []error
	for name, executor := range r {
		if recoverer, ok := executor.(interface{ Recover(context.Context) error }); ok {
			if err := recoverer.Recover(ctx); err != nil {
				failures = append(failures, fmt.Errorf("recover %s: %w", name, err))
			}
		}
	}
	return errors.Join(failures...)
}

// SupportsInteraction delegates capabilities to the selected compute provider.
func (r Runners) SupportsInteraction(task engine.Task) bool {
	provider, ok := r[task.Runner.Type].(engine.InteractiveExecutor)
	return ok && provider.SupportsInteraction(task)
}

func (r Runners) ExecuteTurn(ctx context.Context, task engine.Task, session model.AgentSession, turn model.AgentTurn, controls <-chan engine.SessionControl, emit func(engine.SessionUpdate), log func(string, string)) (engine.TurnResult, error) {
	provider, ok := r[task.Runner.Type].(engine.InteractiveExecutor)
	if !ok || !provider.SupportsInteraction(task) {
		return engine.TurnResult{}, fmt.Errorf("runner %q does not support this interactive worker", task.Runner.Type)
	}
	return provider.ExecuteTurn(ctx, task, session, turn, controls, emit, log)
}

func (r Runners) CloseSession(ctx context.Context, task engine.Task, session model.AgentSession) error {
	provider, ok := r[task.Runner.Type].(engine.InteractiveExecutor)
	if !ok {
		return fmt.Errorf("runner %q cannot close interactive sessions", task.Runner.Type)
	}
	return provider.CloseSession(ctx, task, session)
}
