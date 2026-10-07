package service

import (
	"context"
	"fmt"
	"github.com/wyw14/cry-161/internal/chamber"
	"github.com/wyw14/cry-161/internal/journal"
	"github.com/wyw14/cry-161/internal/model"
	"github.com/wyw14/cry-161/internal/run"
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

func appendRecovery(report RecoveryReport, name string, err error) RecoveryReport {
	action := RecoveryAction{RunID: report.RunID, Name: name, Status: "completed", At: time.Now().UTC()}
	if err != nil {
		action.Status = "failed"
		action.Detail = err.Error()
	}
	report.Actions = append(report.Actions, action)
	return report
}

func (s *Service) Recover(ctx context.Context, runtime *Runtime, runID model.ID) RecoveryReport {
	report := RecoveryReport{RunID: runID, Actions: make([]RecoveryAction, 0), At: time.Now().UTC()}
	value, ok := s.Runs.Get(runID)
	if !ok {
		report = appendRecovery(report, "load_run", fmt.Errorf("run not found"))
		report.NextStep = "verify the run id; no recovery action was taken"
		return report
	}
	report = appendRecovery(report, "load_run", nil)

	// Safety actions must all succeed, in order, before any state change.
	// A failure aborts the transaction: the run keeps its current state so
	// recovery can be retried once the hardware problem is resolved.
	if shutterErr := runtime.Shutter.Close(ctx); shutterErr != nil {
		report = appendRecovery(report, "close_shutter", shutterErr)
		report.NextStep = "retry Recover after the shutter can be closed; source is still active"
		runtime.Timeline.Add(runID, "recovery", "recovered=false failed=close_shutter")
		return report
	}
	report = appendRecovery(report, "close_shutter", nil)

	s.Source.Stop()
	report = appendRecovery(report, "stop_source", nil)

	s.Chamber.SetSourcePower(0)
	if cooldownErr := chamber.DefaultCooldownPolicy().Wait(ctx, s.Chamber); cooldownErr != nil {
		report = appendRecovery(report, "cool_chamber", cooldownErr)
		report.NextStep = "retry Recover once cooling can complete (shutter is closed and source stopped)"
		runtime.Timeline.Add(runID, "recovery", "recovered=false failed=cool_chamber")
		return report
	}
	report = appendRecovery(report, "cool_chamber", nil)

	// Audit must be durable before the run is promoted, otherwise an
	// unrecoverable state could be entered with no recovery record.
	incident := runtime.Incidents.Raise(runID, "warning", "run recovered to safe state")
	if journalErr := s.Journal.Append(journal.IncidentRaised(incident)); journalErr != nil {
		report = appendRecovery(report, "record_recovery", journalErr)
		report.NextStep = "retry Recover to persist the recovery record; hardware is already safe (shutter closed, source stopped, chamber cooled)"
		runtime.Timeline.Add(runID, "recovery", "recovered=false failed=record_recovery")
		return report
	}
	report = appendRecovery(report, "record_recovery", nil)

	// Commit: state transition is gated by the registry. It is the only
	// place run.State is written, so a rejected transition cannot leave
	// the run in cooling without a valid predecessor state.
	if err := run.Advance(s.Runs, runID, model.Cooling); err != nil {
		report = appendRecovery(report, "set_cooling", err)
		report.NextStep = fmt.Sprintf("run is in state %s which cannot recover to cooling; investigate manually", value.State)
		runtime.Timeline.Add(runID, "recovery", "recovered=false failed=set_cooling")
		return report
	}
	report = appendRecovery(report, "set_cooling", nil)
	report.Recovered = true
	report.NextStep = "run is cooling; proceed to venting and release"
	runtime.Timeline.Add(runID, "recovery", "recovered=true")
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
