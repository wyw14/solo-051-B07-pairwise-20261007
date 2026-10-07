package model

import "fmt"

func ValidateRun(run Run) error {
	if run.ID == "" {
		return fmt.Errorf("run id is required")
	}
	if run.Batch == "" {
		return fmt.Errorf("batch is required")
	}
	if run.RecipeID == "" {
		return fmt.Errorf("recipe is required")
	}
	switch run.State {
	case Loading, Pumping, Conditioning, Coating, Cooling, Venting, Released, Failed:
		return nil
	default:
		return fmt.Errorf("unknown state %q", run.State)
	}
}

func CanAdvance(from, to RunState) bool {
	transitions := map[RunState]RunState{Loading: Pumping, Pumping: Conditioning, Conditioning: Coating, Coating: Cooling, Cooling: Venting, Venting: Released}
	if transitions[from] == to {
		return true
	}
	// Recovery may bring an interrupted or failed run straight to cooling,
	// but runs already cooling/venting/released cannot be recovered again.
	if to == Cooling {
		switch from {
		case Loading, Pumping, Conditioning, Coating, Failed:
			return true
		}
	}
	return false
}
