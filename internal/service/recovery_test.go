package service

import (
	"context"
	"testing"

	"github.com/wyw14/cry-161/internal/journal"
	"github.com/wyw14/cry-161/internal/model"
)

func newRecoverableRun(t *testing.T, svc *Service, state model.RunState) model.ID {
	t.Helper()
	runID := model.NewID()
	svc.Runs.Add(&model.Run{ID: runID, Batch: "B-1", RecipeID: model.NewID(), State: model.Loading})
	if state == model.Loading {
		return runID
	}
	path := []model.RunState{model.Pumping, model.Conditioning, model.Coating, model.Cooling, model.Venting, model.Released}
	for _, next := range path {
		if !svc.Runs.Transition(runID, next) {
			t.Fatalf("setup transition to %s failed", next)
		}
		if next == state {
			return runID
		}
	}
	t.Fatalf("unsupported setup state %s", state)
	return runID
}

func TestRecoverSuccessAdvancesToCooling(t *testing.T) {
	t.Setenv("OPTICCOAT_DATA", t.TempDir())
	svc := New()
	runtime := NewRuntime()
	runID := newRecoverableRun(t, svc, model.Coating)

	report := svc.Recover(context.Background(), runtime, runID)

	if !report.Recovered {
		t.Fatalf("expected recovered, got actions: %+v", report.Actions)
	}
	value, ok := svc.Runs.Get(runID)
	if !ok || value.State != model.Cooling {
		t.Fatalf("expected state cooling, got %v (ok=%v)", value, ok)
	}
	if report.NextStep == "" {
		t.Fatal("expected a next step on success")
	}
}

func TestRecoverShutterFailureLeavesStateUnchanged(t *testing.T) {
	t.Setenv("OPTICCOAT_DATA", t.TempDir())
	svc := New()
	runtime := NewRuntime()
	runID := newRecoverableRun(t, svc, model.Coating)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report := svc.Recover(ctx, runtime, runID)

	if report.Recovered {
		t.Fatal("expected recovery to fail when shutter close fails")
	}
	value, _ := svc.Runs.Get(runID)
	if value.State != model.Coating {
		t.Fatalf("state must stay coating, got %s", value.State)
	}
	if report.Actions[len(report.Actions)-1].Name != "close_shutter" {
		t.Fatalf("recovery must abort at close_shutter, got %s", report.Actions[len(report.Actions)-1].Name)
	}
	if report.NextStep == "" {
		t.Fatal("expected an explicit next step")
	}
}

func TestRecoverJournalFailureLeavesStateUnchanged(t *testing.T) {
	t.Setenv("OPTICCOAT_DATA", t.TempDir())
	svc := New()
	svc.Journal = journal.New("/dev/null/not-a-journal-dir")
	runtime := NewRuntime()
	runID := newRecoverableRun(t, svc, model.Coating)

	report := svc.Recover(context.Background(), runtime, runID)

	if report.Recovered {
		t.Fatal("expected recovery to fail when audit record cannot be written")
	}
	value, _ := svc.Runs.Get(runID)
	if value.State != model.Coating {
		t.Fatalf("state must stay coating when journal fails, got %s", value.State)
	}
	if report.Actions[len(report.Actions)-1].Name != "record_recovery" {
		t.Fatalf("recovery must abort at record_recovery, got %s", report.Actions[len(report.Actions)-1].Name)
	}
}

func TestRecoverRejectsRunAlreadyPastCooling(t *testing.T) {
	t.Setenv("OPTICCOAT_DATA", t.TempDir())
	svc := New()
	runtime := NewRuntime()
	runID := newRecoverableRun(t, svc, model.Coating)
	if !svc.Runs.Transition(runID, model.Cooling) {
		t.Fatal("setup cooling transition failed")
	}
	if !svc.Runs.Transition(runID, model.Venting) {
		t.Fatal("setup venting transition failed")
	}

	report := svc.Recover(context.Background(), runtime, runID)

	if report.Recovered {
		t.Fatal("venting run must not be recoverable to cooling")
	}
	value, _ := svc.Runs.Get(runID)
	if value.State != model.Venting {
		t.Fatalf("state must stay venting, got %s", value.State)
	}
	if report.Actions[len(report.Actions)-1].Name != "set_cooling" {
		t.Fatalf("expected failure at set_cooling gate, got %s", report.Actions[len(report.Actions)-1].Name)
	}
}

func TestRecoverUnknownRun(t *testing.T) {
	t.Setenv("OPTICCOAT_DATA", t.TempDir())
	svc := New()
	runtime := NewRuntime()

	report := svc.Recover(context.Background(), runtime, model.NewID())

	if report.Recovered || report.NextStep == "" {
		t.Fatalf("unknown run must fail with a next step, got %+v", report)
	}
}
