package fcctransmitters

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPruneQueryScopedToFetchedStates(t *testing.T) {
	start := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	var q struct {
		Bool struct {
			Filter []map[string]map[string]any `json:"filter"`
		} `json:"bool"`
	}
	if err := json.Unmarshal([]byte(pruneQuery([]string{"RI", "CT"}, start)), &q); err != nil {
		t.Fatal(err)
	}
	if len(q.Bool.Filter) != 2 {
		t.Fatalf("got %d filters, want 2", len(q.Bool.Filter))
	}

	states, _ := q.Bool.Filter[0]["terms"]["state"].([]any)
	if len(states) != 2 || states[0] != "RI" || states[1] != "CT" {
		t.Errorf("terms state = %v, want [RI CT]", states)
	}
	rng, _ := q.Bool.Filter[1]["range"]["fetched_at"].(map[string]any)
	if rng["lt"] != "2026-10-08T03:00:00Z" {
		t.Errorf("range fetched_at = %v, want lt run start", rng)
	}
}

func TestNormalize(t *testing.T) {
	got := normalize([]string{" ri", "", "Ct ", "dtv"})
	want := []string{"RI", "CT", "DTV"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}
