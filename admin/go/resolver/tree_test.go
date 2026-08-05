//go:build !integration

package resolver

import "testing"

func TestTreeLogAdvanced(t *testing.T) {
	tests := []struct {
		name        string
		loadedMTime string
		loadedID    int64
		mtime       string
		id          int64
		want        bool
	}{
		{"nothing changed", "2026-08-05 15:38:56", 100, "2026-08-05 15:38:56", 100, false},
		{"newer second", "2026-08-05 15:38:56", 100, "2026-08-05 15:38:57", 101, true},
		{"same second, new row — the stale-until-next-write window", "2026-08-05 15:38:56", 100, "2026-08-05 15:38:56", 101, true},
		{"older snapshot must not reload", "2026-08-05 15:38:56", 100, "2026-08-05 15:38:55", 99, false},
		{"fresh start loads anything", "", 0, "2026-08-05 15:38:56", 1, true},
	}
	for _, tt := range tests {
		if got := treeLogAdvanced(tt.loadedMTime, tt.loadedID, tt.mtime, tt.id); got != tt.want {
			t.Errorf("%s: treeLogAdvanced(%q, %d, %q, %d) = %v, want %v",
				tt.name, tt.loadedMTime, tt.loadedID, tt.mtime, tt.id, got, tt.want)
		}
	}
}
