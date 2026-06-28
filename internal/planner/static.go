// Package planner provides NextStepPlanner implementations.
// Authoritative: whitepaper §5.3 (StepDecision format), §5.4 (static planner).
package planner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aaronwu000/stateflow/internal/core"
	"gopkg.in/yaml.v3"
)

// stepDef is one entry in the YAML step list (whitepaper §5.4).
type stepDef struct {
	Name           string `yaml:"name"`
	WorkerURL      string `yaml:"worker_url"`
	Mode           string `yaml:"mode"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// staticConfig is the top-level structure of the static planner's YAML config.
type staticConfig struct {
	Steps []stepDef `yaml:"steps"`
}

// StaticPlanner implements NextStepPlanner for a fixed, ordered step list.
//
// It is purely functional: Decide reads only the supplied RunState and the
// immutable step list loaded at construction time. It keeps no counters or
// mutable fields between calls. The same RunState always produces the same
// StepDecision — a property required for safe crash recovery, where the
// orchestrator may re-ask the planner with a reconstructed history and must
// receive the identical answer.
type StaticPlanner struct {
	steps []stepDef // immutable after construction
}

// NewStaticPlanner parses a YAML step list (whitepaper §5.4) and returns a
// ready-to-use StaticPlanner. Returns an error if the YAML is malformed or
// the step list is empty.
//
// Expected YAML shape:
//
//	steps:
//	  - name: ocr
//	    worker_url: http://ocr-service/run
//	    mode: async
//	    timeout_seconds: 30
func NewStaticPlanner(configYAML []byte) (*StaticPlanner, error) {
	var cfg staticConfig
	if err := yaml.Unmarshal(configYAML, &cfg); err != nil {
		return nil, fmt.Errorf("StaticPlanner: parse YAML config: %w", err)
	}
	if len(cfg.Steps) == 0 {
		return nil, fmt.Errorf("StaticPlanner: steps list is empty")
	}
	for i, s := range cfg.Steps {
		if s.Name == "" {
			return nil, fmt.Errorf("StaticPlanner: step[%d] missing name", i)
		}
		if s.WorkerURL == "" {
			return nil, fmt.Errorf("StaticPlanner: step[%d] (%q) missing worker_url", i, s.Name)
		}
		if s.Mode != "sync" && s.Mode != "async" {
			return nil, fmt.Errorf("StaticPlanner: step[%d] (%q) mode must be sync or async, got %q",
				i, s.Name, s.Mode)
		}
	}
	return &StaticPlanner{steps: cfg.Steps}, nil
}

// Decide returns the next step for the run based on how many steps have
// already completed (len(state.History)).
//
//   - len(History) < len(steps): returns status "continue" with the next StepSpec.
//   - len(History) >= len(steps): returns status "done" (all steps complete).
//
// Input assembly (whitepaper §5.3): the static planner is the entity deciding
// both *what* step runs next and *what input it receives*. MVP rule: the worker
// receives the full run context — workflow_input plus all completed step outputs.
// This is intentionally simple; field-level selection is a deferred refinement.
func (p *StaticPlanner) Decide(_ context.Context, state core.RunState) (core.StepDecision, error) {
	n := len(state.History)

	if n >= len(p.steps) {
		return core.StepDecision{Status: "done"}, nil
	}

	def := p.steps[n]

	input, err := buildStepInput(state)
	if err != nil {
		return core.StepDecision{}, fmt.Errorf("StaticPlanner: build input for step %q: %w", def.Name, err)
	}

	return core.StepDecision{
		Status: "continue",
		Step: &core.StepSpec{
			Name:           def.Name,
			WorkerURL:      def.WorkerURL,
			Mode:           def.Mode,
			TimeoutSeconds: def.TimeoutSeconds,
			Input:          input,
		},
	}, nil
}

// stepInput is the JSON payload delivered to each worker by the static planner.
// Contains the original workflow input and the outputs of all previously
// completed steps, in seq order.
type stepInput struct {
	WorkflowInput json.RawMessage     `json:"workflow_input"`
	History       []core.HistoryEntry `json:"history"`
}

func buildStepInput(state core.RunState) (json.RawMessage, error) {
	si := stepInput{
		WorkflowInput: state.WorkflowInput,
		History:       state.History,
	}
	b, err := json.Marshal(si)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
