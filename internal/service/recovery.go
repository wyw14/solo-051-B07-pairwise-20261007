package service

import (
	"context"
	"fmt"
	"github.com/wyw14/cry-161/internal/chamber"
	"github.com/wyw14/cry-161/internal/journal"
	"github.com/wyw14/cry-161/internal/model"
	"github.com/wyw14/cry-161/internal/run"
	"strings"
	"time"
)

type RecoveryAction struct {
	RunID  model.ID  `json:"run_id"`
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

type RecoveryReport struct {
	RunID     model.ID         `json:"run_id"`
	Actions   []RecoveryAction `json:"actions"`
	Recovered bool             `json:"recovered"`
	NextStep  string           `json:"next_step,omitempty"`
	At        time.Time        `json:"at"`
}

// recoveryRemediation maps each recovery step to the operator action required
// when that step fails and the run cannot be advanced.
var recoveryRemediation = map[string]string{
	"close_shutter":   "manually secure the shutter",
	"cool_chamber":    "verify chamber cooldown and interlocks",
	"record_recovery": "resolve journal storage so the event is durably recorded",
	"set_cooling":     "resolve the run state",
}

func appendRecovery(report RecoveryReport, name string, err error) RecoveryReport {
	action := RecoveryAction{RunID: report.RunID, Name: name, Status: "completed", At: time.Now().UTC()}
	if err != nil {
		action.Status = "failed"
		action.Detail = err.Error()
	}
	report.Actions = append(report.Actions, action)
	return report
}

func failedRecoverySteps(report RecoveryReport) []string {
	failed := make([]string, 0)
	for _, action := range report.Actions {
		if action.Status == "failed" {
			failed = append(failed, action.Name)
		}
	}
	return failed
}

// recoveryNextStep explains exactly what an operator must do after a partial
// recovery: the run stays in its previous state until every listed step is
// remediated and recovery is re-run successfully.
func recoveryNextStep(report RecoveryReport, state model.RunState) string {
	steps := failedRecoverySteps(report)
	remediations := make([]string, 0, len(steps))
	for _, name := range steps {
		if fix, ok := recoveryRemediation[name]; ok {
			remediations = append(remediations, fmt.Sprintf("%s (%s)", fix, name))
		} else {
			remediations = append(remediations, name)
		}
	}
	return fmt.Sprintf(
		"run remains in state %s; %s, then re-run recovery — state only advances to cooling once every safety and audit step succeeds",
		state,
		strings.Join(remediations, "; "),
	)
}

func (s *Service) Recover(ctx context.Context, runtime *Runtime, runID model.ID) RecoveryReport {
	report := RecoveryReport{RunID: runID, Actions: make([]RecoveryAction, 0), At: time.Now().UTC()}
	value, ok := s.Runs.Get(runID)
	if !ok {
		report = appendRecovery(report, "load_run", fmt.Errorf("run not found"))
		report.NextStep = "verify the run id and ensure the run is registered before requesting recovery"
		runtime.Timeline.Add(runID, "recovery", "recovered=false reason=run_not_found")
		return report
	}
	state := value.State
	report = appendRecovery(report, "load_run", nil)

	// Safety actions are all attempted even when an earlier one fails, so a
	// faulty shutter never leaves the source powered or the chamber hot.
	shutterErr := runtime.Shutter.Close(ctx)
	report = appendRecovery(report, "close_shutter", shutterErr)
	s.Source.Stop()
	report = appendRecovery(report, "stop_source", nil)
	s.Chamber.SetSourcePower(0)
	cooldownErr := chamber.DefaultCooldownPolicy().Wait(ctx, s.Chamber)
	report = appendRecovery(report, "cool_chamber", cooldownErr)

	message := "run recovered to safe state"
	if shutterErr != nil || cooldownErr != nil {
		message = "run recovery incomplete; safe state not confirmed"
	}
	incident := runtime.Incidents.Raise(runID, "warning", message)
	journalErr := s.Journal.Append(journal.IncidentRaised(incident))
	report = appendRecovery(report, "record_recovery", journalErr)

	// Transactional gate: a failed safety action or a failed audit append
	// aborts the state advance. The run keeps its previous state and the
	// report states precisely which step needs remediation.
	if shutterErr != nil || cooldownErr != nil || journalErr != nil {
		report.Recovered = false
		report.NextStep = recoveryNextStep(report, state)
		runtime.Timeline.Add(runID, "recovery", fmt.Sprintf("recovered=false state=%s incomplete=%s", state, strings.Join(failedRecoverySteps(report), ",")))
		return report
	}

	if err := run.Advance(s.Runs, runID, model.Cooling); err != nil {
		report = appendRecovery(report, "set_cooling", err)
		report.Recovered = false
		report.NextStep = recoveryNextStep(report, state)
		runtime.Timeline.Add(runID, "recovery", fmt.Sprintf("recovered=false state=%s transition=cooling_rejected", state))
		return report
	}
	report = appendRecovery(report, "set_cooling", nil)
	report.Recovered = true
	report.NextStep = "run is cooling in a safe state; wait for cooldown interlocks then vent and evaluate release"
	runtime.Timeline.Add(runID, "recovery", "recovered=true state=cooling")
	return report
}

func (s *Service) Release(runID model.ID, runtime *Runtime, operator string) (model.ReleaseRecord, error) {
	value, ok := s.Runs.Get(runID)
	if !ok {
		return model.ReleaseRecord{}, fmt.Errorf("run not found")
	}
	trace := runtime.Traces.Trace(runID)
	evidence := make([]model.LayerEvidence, 0, len(trace.Thickness))
	for index, sample := range trace.Thickness {
		evidence = append(evidence, model.BuildLayerEvidence(sample, index+1))
	}
	record := model.EvaluateRelease(*value, evidence, runtime.Incidents.ForRun(runID), operator)
	if err := record.Validate(); err != nil {
		return record, err
	}
	if record.Decision != model.ReleaseApproved {
		return record, fmt.Errorf("release rejected")
	}
	if value.State == model.Cooling {
		if err := run.Advance(s.Runs, runID, model.Venting); err != nil {
			return record, err
		}
	}
	return record, nil
}

func (s *Service) Schedule(ctx context.Context, runtime *Runtime, runID model.ID, layers []model.Layer) ([]run.ScheduledLayer, error) {
	scheduler := run.NewScheduler(1)
	for index, layer := range layers {
		if err := scheduler.Enqueue(runID, layer, len(layers)-index); err != nil {
			return nil, err
		}
	}
	err := scheduler.Execute(ctx, func(callCtx context.Context, id model.ID, layer model.Layer) error {
		return s.RunLayer(callCtx, runtime, id, layer)
	})
	return scheduler.Results(), err
}
