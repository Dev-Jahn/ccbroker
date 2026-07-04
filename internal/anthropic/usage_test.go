package anthropic

import "testing"

func TestMaxUtilizationAll(t *testing.T) {
	// nil buckets → 0
	if got := (&Usage{}).MaxUtilizationAll(); got != 0 {
		t.Errorf("empty: got %v want 0", got)
	}

	// no scoped buckets → identical to MaxUtilization
	u := &Usage{FiveHour: &Bucket{Utilization: 0.4}, SevenDay: &Bucket{Utilization: 0.6}}
	if got, want := u.MaxUtilizationAll(), u.MaxUtilization(); got != want {
		t.Errorf("no scoped: got %v want %v", got, want)
	}

	// a scoped weekly bucket above the account windows wins
	u = &Usage{
		FiveHour:     &Bucket{Utilization: 0.2},
		SevenDay:     &Bucket{Utilization: 0.3},
		ScopedWeekly: map[string]Bucket{"Fable": {Utilization: 0.9}},
	}
	if got := u.MaxUtilizationAll(); got != 0.9 {
		t.Errorf("scoped>account: got %v want 0.9", got)
	}
	if got := u.MaxUtilization(); got != 0.3 {
		t.Errorf("MaxUtilization must ignore scoped: got %v want 0.3", got)
	}

	// overage (>1.0) is preserved
	u = &Usage{
		FiveHour:     &Bucket{Utilization: 0.5},
		ScopedWeekly: map[string]Bucket{"X": {Utilization: 1.3}},
	}
	if got := u.MaxUtilizationAll(); got != 1.3 {
		t.Errorf("overage: got %v want 1.3", got)
	}
}
