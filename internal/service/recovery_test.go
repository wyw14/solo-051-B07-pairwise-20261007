package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-161/internal/chamber"
	"github.com/wyw14/cry-161/internal/journal"
	"github.com/wyw14/cry-161/internal/model"
	"github.com/wyw14/cry-161/internal/run"
	"github.com/wyw14/cry-161/internal/source"
)

func newRecoveryService(t *testing.T, journalDir string, state model.RunState) (*Service, *Runtime, model.ID) {
	t.Helper()
	svc := &Service{
		Runs:    run.NewRegistry(),
		Chamber: chamber.New("test-chamber"),
		Source:  source.NewController(100),
		Journal: journal.New(journalDir),
	}
	id := model.ID("run-recover")
	svc.Runs.Add(&model.Run{
		ID:        id,
		Batch:     "batch-1",
		RecipeID:  "recipe-1",
		State:     state,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	return svc, NewRuntime(), id
}

func actionStatus(report RecoveryReport, name string) string {
	for _, action := range report.Actions {
		if action.Name == name {
			return action.Status
		}
	}
	return ""
}

// A failed safety action must leave the run state untouched and name the
// remediation; the run is never force-advanced to cooling.
func TestRecoverShutterFailureKeepsState(t *testing.T) {
	svc, runtime, id := newRecoveryService(t, t.TempDir(), model.Coating)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := svc.Recover(ctx, runtime, id)

	if report.Recovered {
		t.Fatal("expected recovery to fail with cancelled context")
	}
	if actionStatus(report, "close_shutter") != "failed" {
		t.Fatalf("close_shutter status = %q, want failed", actionStatus(report, "close_shutter"))
	}
	stored, _ := svc.Runs.Get(id)
	if stored.State != model.Coating {
		t.Fatalf("state = %s, want coating after failed recovery", stored.State)
	}
	if report.NextStep == "" {
		t.Fatal("expected explicit next step on partial recovery")
	}
}

// A failed audit append must block the state advance even though the hardware
// is in a safe configuration.
func TestRecoverJournalFailureKeepsState(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocker is a regular file, so this directory can never be created and
	// every journal append fails.
	svc, runtime, id := newRecoveryService(t, filepath.Join(blocker, "data"), model.Coating)

	report := svc.Recover(context.Background(), runtime, id)

	if report.Recovered {
		t.Fatal("expected recovery to fail when the journal cannot be written")
	}
	if actionStatus(report, "record_recovery") != "failed" {
		t.Fatalf("record_recovery status = %q, want failed", actionStatus(report, "record_recovery"))
	}
	stored, _ := svc.Runs.Get(id)
	if stored.State != model.Coating {
		t.Fatalf("state = %s, want coating after failed audit", stored.State)
	}
}

// The transition gate runs even after every safety and audit step succeeds:
// an illegal transition must not overwrite the registry.
func TestRecoverIllegalTransitionKeepsState(t *testing.T) {
	svc, runtime, id := newRecoveryService(t, t.TempDir(), model.Pumping)

	report := svc.Recover(context.Background(), runtime, id)

	if report.Recovered {
		t.Fatal("expected recovery to fail on illegal pumping->cooling transition")
	}
	if actionStatus(report, "set_cooling") != "failed" {
		t.Fatalf("set_cooling status = %q, want failed", actionStatus(report, "set_cooling"))
	}
	stored, _ := svc.Runs.Get(id)
	if stored.State != model.Pumping {
		t.Fatalf("state = %s, want pumping after rejected transition", stored.State)
	}
}

// Only the fully successful transaction advances the run to cooling, via the
// registry's transition gate.
func TestRecoverSuccessAdvancesToCooling(t *testing.T) {
	svc, runtime, id := newRecoveryService(t, t.TempDir(), model.Coating)

	report := svc.Recover(context.Background(), runtime, id)

	if !report.Recovered {
		t.Fatalf("expected successful recovery, actions: %+v", report.Actions)
	}
	stored, _ := svc.Runs.Get(id)
	if stored.State != model.Cooling {
		t.Fatalf("state = %s, want cooling", stored.State)
	}
}

func TestRecoverMissingRun(t *testing.T) {
	svc, runtime, _ := newRecoveryService(t, t.TempDir(), model.Coating)

	report := svc.Recover(context.Background(), runtime, model.ID("missing"))

	if report.Recovered {
		t.Fatal("missing run must not report recovery")
	}
	if report.NextStep == "" {
		t.Fatal("expected guidance for a missing run")
	}
}
