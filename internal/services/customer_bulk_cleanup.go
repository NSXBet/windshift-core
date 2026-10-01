package services

import (
	"context"
	"fmt"
	"time"

	"windshift/internal/database"
	"windshift/internal/logger"
	"windshift/internal/models"
)

// Bulk cleanup bounds for the email-intake provenance story (WI-1553). A
// single run erases at most MaxBulkCleanupBatch customers — accidental
// wide-net runs stay reviewable, and repeating the call drains the backlog.
const (
	MaxBulkCleanupBatch          = 1000
	DefaultBulkCleanupBatchLimit = 200
)

// CustomerBulkCleanupInput selects the auto-created customers a cleanup run
// targets. CreatedVia is optional (empty = any provenance); OlderThanDays is
// optional (0 = no age filter). Customers holding any ticket content — items,
// comments, or attachments — are always excluded.
type CustomerBulkCleanupInput struct {
	CreatedVia    string
	OlderThanDays int
}

// CustomerBulkCleanupSkipped records one candidate the run did not erase.
type CustomerBulkCleanupSkipped struct {
	ID     int    `json:"id"`
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

// CustomerBulkCleanupErased records one completed erasure.
type CustomerBulkCleanupErased struct {
	ID    int    `json:"id"`
	Email string `json:"email"`
}

// CustomerBulkCleanupResult is the run report: what was erased, what was
// skipped and why, and what failed. It is also what the single batch audit
// event records.
type CustomerBulkCleanupResult struct {
	Erased       []CustomerBulkCleanupErased  `json:"erased"`
	Skipped      []CustomerBulkCleanupSkipped `json:"skipped"`
	Failed       []CustomerBulkCleanupSkipped `json:"failed"`
	ScannedCount int                          `json:"scanned_count"`
}

// ValidateBulkCleanupCreatedVia reports whether createdVia is an accepted
// filter value ("" = any).
func ValidateBulkCleanupCreatedVia(createdVia string) bool {
	if createdVia == "" {
		return true
	}
	for _, v := range models.CustomerCreatedViaValues {
		if v == createdVia {
			return true
		}
	}
	return false
}

// CleanupAutoCreatedCustomers erases stale auto-created portal customers that
// never became tickets, reusing the WI-1550 erasure flow per customer so
// every individual erasure keeps its DSAR evidence record. A single batch
// audit event lists what was erased and skipped. Ticket-holding customers are
// excluded by construction — the whole point is cleaning up spam and
// one-off senders that never interacted.
func CleanupAutoCreatedCustomers(db database.Database, actor AuditActor, input CustomerBulkCleanupInput) (CustomerBulkCleanupResult, error) {
	var result CustomerBulkCleanupResult

	if !ValidateBulkCleanupCreatedVia(input.CreatedVia) {
		return result, NewServiceError(400, "created_via must be one of: agent, email-intake, magic-link, ticket-import, unknown")
	}
	if input.OlderThanDays < 0 {
		return result, NewServiceError(400, "older_than_days must be >= 0")
	}

	limit := DefaultBulkCleanupBatchLimit
	cutoff := time.Now()
	if input.OlderThanDays > 0 {
		cutoff = cutoff.Add(-time.Duration(input.OlderThanDays) * 24 * time.Hour)
	} else {
		// No age filter: only consider rows up to now (identical semantics,
		// keeps the query shape single).
		cutoff = cutoff.Add(time.Hour) // future-dated created_at rows are anomalies; exclude them
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	candidates, err := selectCleanupCandidates(ctx, db, input.CreatedVia, cutoff, limit)
	if err != nil {
		return result, err
	}
	result.ScannedCount = len(candidates)

	for _, candidate := range candidates {
		// Safety boundary: anything that became ticket content — as a
		// requester, commenter, or uploader — is skipped with a reason.
		if excluded, reason, err := hasTicketContent(ctx, db, candidate.ID); err != nil {
			result.Failed = append(result.Failed, CustomerBulkCleanupSkipped{ID: candidate.ID, Email: candidate.Email, Reason: err.Error()})
			continue
		} else if excluded {
			result.Skipped = append(result.Skipped, CustomerBulkCleanupSkipped{ID: candidate.ID, Email: candidate.Email, Reason: reason})
			continue
		}

		intake := CustomerErasureInput{
			RequestedBy: fmt.Sprintf("bulk-cleanup created_via=%s admin:%s", input.CreatedVia, actor.Username),
			Notes:       "Bulk cleanup of stale auto-created customers (WI-1553)",
		}
		if _, err := EraseCustomer(db, candidate.ID, actor, intake); err != nil {
			// The erasure flow guards its own state (404/409); a failure here
			// is recorded per customer and never aborts the batch.
			result.Failed = append(result.Failed, CustomerBulkCleanupSkipped{ID: candidate.ID, Email: candidate.Email, Reason: err.Error()})
			continue
		}
		result.Erased = append(result.Erased, CustomerBulkCleanupErased{ID: candidate.ID, Email: candidate.Email})
	}

	// One audit event for the whole batch, mirroring the WI-1550 erasure
	// audit style: actor, subject list, and the selection parameters.
	details := map[string]any{
		"created_via":      input.CreatedVia,
		"older_than_days":  input.OlderThanDays,
		"erased_count":     len(result.Erased),
		"erased_customers": eraseListDetails(result.Erased),
		"skipped":          skippedListDetails(result.Skipped),
		"failed":           skippedListDetails(result.Failed),
	}
	customerID := 0
	_ = logger.LogAudit(db, logger.AuditEvent{
		UserID:       actor.UserID,
		Username:     actor.Username,
		IPAddress:    actor.IPAddress,
		UserAgent:    actor.UserAgent,
		ActionType:   logger.ActionPortalCustomerBulkCleanup,
		ResourceType: logger.ResourcePortalCustomer,
		ResourceID:   &customerID,
		Success:      true,
		Details:      details,
	})

	return result, nil
}

type bulkCleanupCandidate struct {
	ID    int
	Name  string
	Email string
}

// selectCleanupCandidates loads stale customers matching the provenance and
// age filters (erased rows excluded). Ticket-content boundaries are checked
// per candidate so the run report can show exactly why each holdout was
// skipped.
func selectCleanupCandidates(ctx context.Context, db database.Database, createdVia string, cutoff time.Time, limit int) ([]bulkCleanupCandidate, error) {
	query := `
		SELECT pc.id, pc.name, pc.email
		FROM portal_customers pc
		WHERE pc.erased_at IS NULL
		  AND pc.created_at < ?
	`
	args := []any{cutoff}
	if createdVia != "" {
		query += ` AND pc.created_via = ?`
		args = append(args, createdVia)
	}
	query += ` ORDER BY pc.id LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select cleanup candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []bulkCleanupCandidate
	for rows.Next() {
		var c bulkCleanupCandidate
		if err := rows.Scan(&c.ID, &c.Name, &c.Email); err != nil {
			return nil, fmt.Errorf("scan cleanup candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// customerTicketFootprint counts the ticket-content markers that exclude a
// customer from cleanup.
func customerTicketFootprint(ctx context.Context, db database.Database, customerID int) (items, comments, attachments int, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM items WHERE creator_portal_customer_id = ?),
			(SELECT COUNT(*) FROM comments WHERE portal_customer_id = ?),
			(SELECT COUNT(*) FROM attachments WHERE uploaded_by_portal_customer_id = ?)
	`, customerID, customerID, customerID).Scan(&items, &comments, &attachments)
	return
}

// hasTicketContent reports whether the customer authored or requested ticket
// content — the hard exclusion boundary for cleanup (WI-1553).
func hasTicketContent(ctx context.Context, db database.Database, customerID int) (excluded bool, reason string, err error) {
	items, comments, attachments, err := customerTicketFootprint(ctx, db, customerID)
	if err != nil {
		return false, "", fmt.Errorf("check ticket content: %w", err)
	}
	switch {
	case items > 0:
		return true, "has_items", nil
	case comments > 0:
		return true, "has_comments", nil
	case attachments > 0:
		return true, "has_attachments", nil
	default:
		return false, "", nil
	}
}

func eraseListDetails(erased []CustomerBulkCleanupErased) []map[string]any {
	out := make([]map[string]any, 0, len(erased))
	for _, e := range erased {
		out = append(out, map[string]any{"id": e.ID, "email": e.Email})
	}
	return out
}

func skippedListDetails(skipped []CustomerBulkCleanupSkipped) []map[string]any {
	out := make([]map[string]any, 0, len(skipped))
	for _, s := range skipped {
		out = append(out, map[string]any{"id": s.ID, "email": s.Email, "reason": s.Reason})
	}
	return out
}
