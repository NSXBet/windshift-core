package wscli

import "testing"

func TestNormalizeMilestoneStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"canceled":    "cancelled", //nolint:misspell // Persisted API status.
		"Canceled":    "cancelled", //nolint:misspell // Persisted API status.
		"cancelled":   "cancelled", //nolint:misspell // Persisted API status.
		"in-progress": "in-progress",
		"":            "",
	}
	for in, want := range cases {
		if got := normalizeMilestoneStatus(in); got != want {
			t.Errorf("normalizeMilestoneStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
