package scm

import (
	"context"
	"net/http"
	"testing"
	"time"

	"windshift/internal/models"
)

// GitHub's list endpoint omits `merged` and only carries `merged_at`
// (Windshiftapp/core#304). A listed merged PR must still read as merged, or
// sync stores it as "closed" and never emits scm_pr_merged.
func TestGitHubListPullRequestsDetectsMergedFromMergedAt(t *testing.T) {
	t.Parallel()
	provider, err := NewGitHubProvider(ProviderConfig{AuthMethod: models.SCMAuthMethodPAT, PersonalAccessToken: "pat-token"})
	if err != nil {
		t.Fatal(err)
	}
	provider.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/repos/acme/app/pulls" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
		// Shapes as returned by GET /repos/{owner}/{repo}/pulls: no "merged" key.
		return jsonResponse(http.StatusOK, `[
			{"id": 1, "number": 11, "state": "closed", "merged_at": "2026-10-01T12:00:00Z", "closed_at": "2026-10-01T12:00:00Z"},
			{"id": 2, "number": 12, "state": "closed", "merged_at": null, "closed_at": "2026-10-01T12:00:00Z"},
			{"id": 3, "number": 13, "state": "open", "merged_at": null}
		]`), nil
	})}

	prs, err := provider.ListPullRequests(context.Background(), "acme", "app", ListPROptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 {
		t.Fatalf("got %d pull requests, want 3", len(prs))
	}

	merged, closed, open := prs[0], prs[1], prs[2]
	if !merged.IsMerged {
		t.Errorf("PR #%d with merged_at set: IsMerged = false, want true", merged.Number)
	}
	if want := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC); merged.MergedAt == nil || !merged.MergedAt.Equal(want) {
		t.Errorf("PR #%d MergedAt = %v, want %v", merged.Number, merged.MergedAt, want)
	}
	if closed.IsMerged {
		t.Errorf("PR #%d closed without merge: IsMerged = true, want false", closed.Number)
	}
	if open.IsMerged {
		t.Errorf("PR #%d open: IsMerged = true, want false", open.Number)
	}
}

// The single-PR endpoint does send `merged`; it keeps working on its own.
func TestGitHubPullRequestMergedFlagStillHonoured(t *testing.T) {
	t.Parallel()
	pr := githubPullRequest{Number: 21, State: "closed", Merged: true}
	if !pr.toPullRequest().IsMerged {
		t.Fatal("merged=true without merged_at: IsMerged = false, want true")
	}
}
