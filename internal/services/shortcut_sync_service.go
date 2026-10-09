package services

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/repository"
	"windshift/internal/validation"
)

// ShortcutSyncService implements the core half of the Shortcut plugin
// item_upsert/item_lookup wire contract (shortcut-v1-contracts.md §1).
// Items are reconciled through the canonical external item reconciliation
// path so validation, history, and side-effect suppression stay consistent.
type ShortcutSyncService struct {
	db database.Database
}

func NewShortcutSyncService(db database.Database) *ShortcutSyncService {
	return &ShortcutSyncService{db: db}
}

// ShortcutItemUpsertRequest is the decoded item_upsert payload.
type ShortcutItemUpsertRequest struct {
	ExternalKind       string
	ExternalID         int64
	ExternalURL        string
	ExternalUpdatedAt  string // RFC3339 UTC
	WorkspaceID        string // decimal string of the workspace id
	Title              string
	Description        string
	StatusName         string
	ItemTypeName       string
	PriorityName       string
	ProjectName        string // find-only
	DueDate            string // YYYY-MM-DD
	StoryPoints        *float64
	Labels             []string // find-or-create
	LabelMode          string   // "merge" or "replace" (default)
	ParentExternalKind string
	ParentExternalID   int64
	// AssigneeExternalID maps to items.assignee_id (first Shortcut owner
	// wins; Shortcut's column allows one; the id is Shortcut's member UUID).
	// IterationExternalID maps to
	// items.iteration_id (the iteration's own workspace must match the
	// item's). Both resolve through the sync's own mapping tables; a missing
	// mapping silently drops the assignment rather than failing the story.
	AssigneeExternalID  string
	IterationExternalID int64
}

// ShortcutSyncUpsertResult reports the reconciled item.
type ShortcutSyncUpsertResult struct {
	ItemID  int
	ItemKey string
	Created bool
}

// ShortcutSyncLookupResult reports a stored mapping.
type ShortcutSyncLookupResult struct {
	ItemID            int
	ItemKey           string
	ExternalUpdatedAt *time.Time
	LastSyncedAt      *time.Time
}

var shortcutExternalKinds = map[string]bool{"story": true, "epic": true}

func (s *ShortcutSyncService) Upsert(ctx context.Context, req ShortcutItemUpsertRequest) (ShortcutSyncUpsertResult, error) {
	if !shortcutExternalKinds[req.ExternalKind] {
		return ShortcutSyncUpsertResult{}, &validation.ValidationError{Field: "external_kind", Message: "must be \"story\" or \"epic\""}
	}
	if req.ExternalID <= 0 {
		return ShortcutSyncUpsertResult{}, &validation.ValidationError{Field: "external_id", Message: "must be positive"}
	}
	if strings.TrimSpace(req.Title) == "" {
		return ShortcutSyncUpsertResult{}, &validation.ValidationError{Field: "title", Message: "is required"}
	}
	switch req.LabelMode {
	case "", "merge", "replace":
	default:
		return ShortcutSyncUpsertResult{}, &validation.ValidationError{Field: "label_mode", Message: "must be \"merge\" or \"replace\""}
	}
	workspaceID, err := s.resolveWorkspaceID(req.WorkspaceID)
	if err != nil {
		return ShortcutSyncUpsertResult{}, err
	}
	if _, err := parseShortcutTimestamp(req.ExternalUpdatedAt); err != nil {
		return ShortcutSyncUpsertResult{}, &validation.ValidationError{Field: "external_updated_at", Message: "must be RFC3339 UTC"}
	}
	var dueDate *time.Time
	if req.DueDate != "" {
		parsed, err := time.Parse("2006-01-02", req.DueDate)
		if err != nil {
			return ShortcutSyncUpsertResult{}, &validation.ValidationError{Field: "due_date", Message: "must be YYYY-MM-DD"}
		}
		dueDate = &parsed
	}

	var itemID int
	var created bool
	mapping, err := s.findMapping(req.ExternalKind, req.ExternalID)
	if err != nil {
		return ShortcutSyncUpsertResult{}, err
	}
	if mapping == nil {
		itemID, err = s.createItem(ctx, workspaceID, req, dueDate)
		if err == nil {
			created = true
		}
	} else {
		itemID = mapping.itemID
		err = s.updateItem(ctx, mapping.itemID, req, dueDate)
	}
	if err != nil {
		return ShortcutSyncUpsertResult{}, err
	}

	itemKey, err := repository.NewItemRepository(s.db).GetItemKey(itemID)
	if err != nil {
		return ShortcutSyncUpsertResult{}, fmt.Errorf("shortcut sync: resolve item key: %w", err)
	}
	return ShortcutSyncUpsertResult{ItemID: itemID, ItemKey: itemKey, Created: created}, nil
}

func (s *ShortcutSyncService) Lookup(_ context.Context, externalKind string, externalID int64) (*ShortcutSyncLookupResult, error) {
	if !shortcutExternalKinds[externalKind] {
		return nil, &validation.ValidationError{Field: "external_kind", Message: "must be \"story\" or \"epic\""}
	}
	if externalID <= 0 {
		return nil, &validation.ValidationError{Field: "external_id", Message: "must be positive"}
	}
	mapping, err := s.findMapping(externalKind, externalID)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return nil, nil
	}
	itemKey, err := repository.NewItemRepository(s.db).GetItemKey(mapping.itemID)
	if err != nil {
		return nil, fmt.Errorf("shortcut sync: resolve item key: %w", err)
	}
	return &ShortcutSyncLookupResult{
		ItemID:            mapping.itemID,
		ItemKey:           itemKey,
		ExternalUpdatedAt: mapping.externalUpdatedAt,
		LastSyncedAt:      mapping.lastSyncedAt,
	}, nil
}

type shortcutMapping struct {
	itemID            int
	externalUpdatedAt *time.Time
	lastSyncedAt      *time.Time
}

func (s *ShortcutSyncService) findMapping(externalKind string, externalID int64) (*shortcutMapping, error) {
	var m shortcutMapping
	var externalUpdatedAt, lastSyncedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT item_id, external_updated_at, last_synced_at
		FROM shortcut_sync_items
		WHERE external_kind = ? AND external_id = ?
	`, externalKind, externalID).Scan(&m.itemID, &externalUpdatedAt, &lastSyncedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shortcut sync: find mapping: %w", err)
	}
	if externalUpdatedAt.Valid {
		m.externalUpdatedAt = &externalUpdatedAt.Time
	}
	if lastSyncedAt.Valid {
		m.lastSyncedAt = &lastSyncedAt.Time
	}
	return &m, nil
}

func (s *ShortcutSyncService) createItem(ctx context.Context, workspaceID int, req ShortcutItemUpsertRequest, dueDate *time.Time) (int, error) {
	var itemTypeID *int
	if req.ItemTypeName != "" {
		id, err := s.resolveNamedID("item_types", req.ItemTypeName)
		if err != nil {
			return 0, err
		}
		itemTypeID = &id
	}

	var statusID *int
	if req.StatusName != "" {
		id, err := s.resolveNamedID("statuses", req.StatusName)
		if err != nil {
			return 0, err
		}
		statusID = &id
	} else {
		var defaultStatusID int
		err := s.db.QueryRow("SELECT id FROM statuses WHERE is_default = true LIMIT 1").Scan(&defaultStatusID)
		if err != nil {
			return 0, fmt.Errorf("shortcut sync: resolve default status: %w", err)
		}
		statusID = &defaultStatusID
	}

	var priorityID *int
	if req.PriorityName != "" {
		id, err := s.resolveNamedID("priorities", req.PriorityName)
		if err != nil {
			return 0, err
		}
		priorityID = &id
	}

	var projectID *int
	if req.ProjectName != "" {
		// Find-only: a missing Windshift project never blocks creation.
		projectID = s.findProjectID(req.ProjectName)
	}

	parentID, err := s.resolveParentItemID(req.ParentExternalKind, req.ParentExternalID)
	if err != nil {
		return 0, err
	}

	var assigneeID, iterationID *int
	if req.AssigneeExternalID != "" {
		id, err := s.resolveAssigneeID(req.AssigneeExternalID)
		if err != nil {
			return 0, err
		}
		assigneeID = id
	}
	if req.IterationExternalID > 0 {
		id, err := s.resolveIterationID(req.IterationExternalID, workspaceID)
		if err != nil {
			return 0, err
		}
		iterationID = id
	}

	now := time.Now().UTC()
	item, err := NewExternalItemReconciliationService(s.db).Create(ctx, ExternalItemCreateRequest{
		Policy: ShortcutReconciliationPolicy(),
		Input: ItemCreateInput{
			WorkspaceID: workspaceID,
			Title:       req.Title,
			Description: req.Description,
			StatusID:    statusID,
			PriorityID:  priorityID,
			ItemTypeID:  itemTypeID,
			ProjectID:   projectID,
			ParentID:    parentID,
			DueDate:     dueDate,
			StoryPoints: req.StoryPoints,
			AssigneeID:  assigneeID,
			IterationID: iterationID,
		},
		AfterCreate: func(ctx context.Context, tx database.Tx, itemID int) error {
			if err := s.applyLabelsTx(ctx, tx, itemID, req.Labels, req.LabelMode); err != nil {
				return err
			}
			_, err := tx.Exec(`
				INSERT INTO shortcut_sync_items (
					external_kind, external_id, item_id, external_url,
					external_updated_at, last_synced_at, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, req.ExternalKind, req.ExternalID, itemID, req.ExternalURL,
				externalUpdatedAtOrNil(req.ExternalUpdatedAt), now, now, now)
			if err != nil {
				return fmt.Errorf("shortcut sync: insert mapping: %w", err)
			}
			return nil
		},
	})
	if err != nil {
		return 0, fmt.Errorf("shortcut sync: create item: %w", err)
	}
	return item.ID, nil
}

func (s *ShortcutSyncService) updateItem(ctx context.Context, itemID int, req ShortcutItemUpsertRequest, dueDate *time.Time) error {
	updateData := map[string]any{}
	if req.Title != "" {
		updateData["title"] = req.Title
	}
	if req.Description != "" {
		updateData["description"] = req.Description
	}
	if req.StatusName != "" {
		id, err := s.resolveNamedID("statuses", req.StatusName)
		if err != nil {
			return err
		}
		updateData["status_id"] = id
	}
	if req.PriorityName != "" {
		id, err := s.resolveNamedID("priorities", req.PriorityName)
		if err != nil {
			return err
		}
		updateData["priority_id"] = id
	}
	if dueDate != nil {
		updateData["due_date"] = req.DueDate
	}
	if req.StoryPoints != nil {
		updateData["story_points"] = *req.StoryPoints
	}
	if req.ProjectName != "" {
		// Find-only on update too: missing project is skipped, not an error.
		if id := s.findProjectID(req.ProjectName); id != nil {
			updateData["project_id"] = *id
		}
	}
	if req.ParentExternalKind != "" {
		parentID, err := s.resolveParentItemID(req.ParentExternalKind, req.ParentExternalID)
		if err != nil {
			return err
		}
		if parentID != nil {
			// CoerceInt rejects *int, so deref before the any-typed map
			// hands the value to the update validator.
			updateData["parent_id"] = *parentID
		}
	}
	if req.AssigneeExternalID != "" {
		if id, err := s.resolveAssigneeID(req.AssigneeExternalID); err != nil {
			return err
		} else if id != nil {
			updateData["assignee_id"] = *id
		}
	}
	if req.IterationExternalID > 0 {
		workspaceID, wsErr := s.resolveWorkspaceID(req.WorkspaceID)
		if wsErr != nil {
			return wsErr
		}
		if id, err := s.resolveIterationID(req.IterationExternalID, workspaceID); err != nil {
			return err
		} else if id != nil {
			updateData["iteration_id"] = *id
		}
	}

	now := time.Now().UTC()
	_, err := NewExternalItemReconciliationService(s.db).Update(ctx, ExternalItemUpdateRequest{
		Policy:     ShortcutReconciliationPolicy(),
		ItemID:     itemID,
		UpdateData: updateData,
		AfterUpdate: func(ctx context.Context, tx database.Tx, _, updated *models.Item) error {
			if err := s.applyLabelsTx(ctx, tx, itemID, req.Labels, req.LabelMode); err != nil {
				return err
			}
			_, err := tx.Exec(`
				UPDATE shortcut_sync_items
				SET external_url = ?, external_updated_at = ?, last_synced_at = ?, updated_at = ?
				WHERE item_id = ?
			`, req.ExternalURL, externalUpdatedAtOrNil(req.ExternalUpdatedAt), now, now, itemID)
			if err != nil {
				return fmt.Errorf("shortcut sync: refresh mapping: %w", err)
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("shortcut sync: update item %d: %w", itemID, err)
	}
	return nil
}

func (s *ShortcutSyncService) applyLabelsTx(ctx context.Context, tx database.Tx, itemID int, names []string, mode string) error {
	switch mode {
	case "", "replace", "merge":
	default:
		return &validation.ValidationError{Field: "label_mode", Message: "must be \"merge\" or \"replace\""}
	}
	labelRepo := repository.NewLabelRepository(s.db)
	labelIDs := make([]int, 0, len(names))
	if mode == "merge" {
		rows, err := tx.Query(`SELECT label_id FROM item_labels WHERE item_id = ?`, itemID)
		if err != nil {
			return fmt.Errorf("shortcut sync: read existing labels: %w", err)
		}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("shortcut sync: read existing labels: %w", err)
			}
			labelIDs = append(labelIDs, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("shortcut sync: read existing labels: %w", err)
		}
		_ = rows.Close()
	}
	seen := make(map[int]bool, len(labelIDs))
	for _, id := range labelIDs {
		seen[id] = true
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		labelID, err := labelRepo.EnsureByNameTx(ctx, tx, name, "")
		if err != nil {
			return fmt.Errorf("shortcut sync: ensure label %q: %w", name, err)
		}
		if !seen[labelID] {
			seen[labelID] = true
			labelIDs = append(labelIDs, labelID)
		}
	}
	if err := labelRepo.ReplaceItemLabelsTx(ctx, tx, itemID, labelIDs); err != nil {
		return fmt.Errorf("shortcut sync: replace item labels: %w", err)
	}
	return nil
}

func (s *ShortcutSyncService) resolveNamedID(table, name string) (int, error) {
	query := fmt.Sprintf("SELECT id FROM %s WHERE name = ? LIMIT 1", table)
	var id int
	err := s.db.QueryRow(query, name).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, &validation.ValidationError{Field: table, Message: fmt.Sprintf("no %s named %q", table, name)}
	}
	if err != nil {
		return 0, fmt.Errorf("shortcut sync: resolve %s: %w", table, err)
	}
	return id, nil
}

func (s *ShortcutSyncService) findProjectID(name string) *int {
	var id int
	err := s.db.QueryRow("SELECT id FROM time_projects WHERE name = ? LIMIT 1", name).Scan(&id)
	if err != nil {
		return nil
	}
	return &id
}

// resolveAssigneeID maps a Shortcut member id to a windshift user via
// shortcut_sync_users: users are imported on demand (member_upsert wire),
// never invented from a bare id. A missing mapping is a silent drop — an
// assignment must never block the story it decorates.
func (s *ShortcutSyncService) resolveAssigneeID(externalID string) (*int, error) {
	var userID int
	err := s.db.QueryRow(`SELECT user_id FROM shortcut_sync_users WHERE external_id = ?`, externalID).Scan(&userID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shortcut sync: find user mapping %s: %w", externalID, err)
	}
	return &userID, nil
}

// resolveIterationID maps a Shortcut iteration id to a windshift iteration
// via shortcut_sync_iterations. Iterations are imported on demand
// (iteration_upsert wire) and are workspace-scoped in windshift, so the
// mapping must match the item's workspace. A missing mapping is a silent
// drop, same policy as assignees.
func (s *ShortcutSyncService) resolveIterationID(externalID int64, workspaceID int) (*int, error) {
	var iterationID int
	err := s.db.QueryRow(`
		SELECT iteration_id FROM shortcut_sync_iterations
		WHERE external_id = ? AND workspace_id = ?`, externalID, workspaceID).Scan(&iterationID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shortcut sync: find iteration mapping %d: %w", externalID, err)
	}
	return &iterationID, nil
}

func (s *ShortcutSyncService) resolveParentItemID(externalKind string, externalID int64) (*int, error) {
	if externalKind == "" {
		return nil, nil
	}
	if !shortcutExternalKinds[externalKind] {
		return nil, &validation.ValidationError{Field: "parent_external_kind", Message: "must be \"story\" or \"epic\""}
	}
	mapping, err := s.findMapping(externalKind, externalID)
	if err != nil {
		return nil, err
	}
	if mapping == nil {
		return nil, &validation.ValidationError{Field: "parent_external_id", Message: "parent mapping not found"}
	}
	return &mapping.itemID, nil
}

// ShortcutMemberUpsertRequest is the member_upsert wire payload: one
// Shortcut member, imported on demand when a story carries their assignment.
type ShortcutMemberUpsertRequest struct {
	ExternalID   string // Shortcut member id (UUID, from stories' owner_ids)
	Name         string // full name as shown in Shortcut
	Email        string // Shortcut account email (unique in windshift)
	UsernameHint string // optional; username hint when the email is unusable
}

// ShortcutMemberUpsertResult reports the imported user.
type ShortcutMemberUpsertResult struct {
	UserID  int  `json:"user_id"`
	Created bool `json:"created,omitempty"`
}

// UpsertMember imports a Shortcut member as an active windshift user keyed by
// the sync's user mapping. Idempotent: the mapping short-circuits, and the
// email UNIQUE doubles as a second idempotency line when a prior import left
// the user without its mapping row. Names are derived from the email local
// part when Shortcut sent no usable display name. Passwords stay unset —
// these users only own work, they cannot log in until an admin resets.
func (s *ShortcutSyncService) UpsertMember(ctx context.Context, req ShortcutMemberUpsertRequest) (ShortcutMemberUpsertResult, error) {
	if req.ExternalID == "" {
		return ShortcutMemberUpsertResult{}, &validation.ValidationError{Field: "external_id", Message: "must be a Shortcut member id"}
	}
	name := strings.TrimSpace(req.Name)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" {
		// Sync-sourced logins must never collide with real accounts: derive a
		// unique synthetic email when Shortcut sent none. The SyncMember at
		// the start of the username mirrors the SyncMachine pattern the
		// device-import path uses for machines.
		hint := strings.TrimSpace(req.UsernameHint)
		if hint == "" {
			if name != "" {
				hint = name
			} else {
				hint = fmt.Sprintf("member-%s", req.ExternalID)
			}
		}
		email = fmt.Sprintf("sync-member-%s@shortcut.import", req.ExternalID)
	}
	if name == "" {
		name = email
	}

	var userID int
	err := s.db.QueryRow(`SELECT user_id FROM shortcut_sync_users WHERE external_id = ?`, req.ExternalID).Scan(&userID)
	if err == nil {
		return ShortcutMemberUpsertResult{UserID: userID}, nil
	}
	if err != sql.ErrNoRows {
		return ShortcutMemberUpsertResult{}, fmt.Errorf("shortcut sync: find user mapping %s: %w", req.ExternalID, err)
	}

	firstName, lastName := splitPersonName(name)
	now := time.Now().UTC()
	var passwordHash *string
	inserted := false
	err = database.WithTx(s.db, func(tx database.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO users (email, username, first_name, last_name, password_hash, is_active, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, TRUE, ?, ?)
			ON CONFLICT(email) DO NOTHING`,
			email, email, firstName, lastName, passwordHash, now, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		if inserted {
			id, idErr := res.LastInsertId()
			if idErr != nil {
				return idErr
			}
			userID = int(id)
			return nil
		}
		// Lost the email-unique race (or a stale mapping row): adopt the
		// existing account.
		return tx.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&userID)
	})
	if err != nil {
		return ShortcutMemberUpsertResult{}, fmt.Errorf("shortcut sync: import member %s: %w", req.ExternalID, err)
	}
	if !inserted {
		if err := s.db.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&userID); err != nil {
			return ShortcutMemberUpsertResult{}, fmt.Errorf("shortcut sync: adopt member %s: %w", req.ExternalID, err)
		}
	}
	if _, err := s.db.Exec(`
		INSERT INTO shortcut_sync_users (external_id, user_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(external_id) DO NOTHING`, req.ExternalID, userID, now, now); err != nil {
		return ShortcutMemberUpsertResult{}, fmt.Errorf("shortcut sync: insert user mapping: %w", err)
	}
	return ShortcutMemberUpsertResult{UserID: userID, Created: inserted}, nil
}

// splitPersonName splits a display name into first/last with the overflow
// going to the last name ("Ana Paula Souza" → Ana / Paula Souza).
func splitPersonName(name string) (string, string) {
	parts := strings.Fields(name)
	switch len(parts) {
	case 0:
		return "Sync", "Member"
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

// ShortcutIterationUpsertRequest is the iteration_upsert wire payload: one
// Shortcut iteration, imported on demand when a story references it.
type ShortcutIterationUpsertRequest struct {
	ExternalID int64 // Shortcut iteration id (stories' iteration_id entries)
	Name       string
	StartDate  string // YYYY-MM-DD
	EndDate    string // YYYY-MM-DD
	Status     string // Shortcut state ("unstarted"|"started"|"done"); empty → date-derived
	Workspace  string // decimal workspace id the iteration belongs to
}

// ShortcutIterationUpsertResult reports the imported iteration.
type ShortcutIterationUpsertResult struct {
	IterationID int  `json:"iteration_id"`
	Created     bool `json:"created,omitempty"`
}

// UpsertIteration imports a Shortcut iteration as a workspace-local windshift
// iteration of the seeded Sprint type, keyed by the sync's iteration mapping.
// Idempotent: the mapping short-circuits. Status derives from Shortcut's own
// state plus the wall clock, so past iterations import as completed even when
// Shortcut forgot to close them.
func (s *ShortcutSyncService) UpsertIteration(ctx context.Context, req ShortcutIterationUpsertRequest) (ShortcutIterationUpsertResult, error) {
	if req.ExternalID <= 0 {
		return ShortcutIterationUpsertResult{}, &validation.ValidationError{Field: "external_id", Message: "must be a positive Shortcut iteration id"}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ShortcutIterationUpsertResult{}, &validation.ValidationError{Field: "name", Message: "is required"}
	}
	start, end, err := normalizeIterationDates(req.StartDate, req.EndDate)
	if err != nil {
		return ShortcutIterationUpsertResult{}, err
	}
	workspaceID, err := s.resolveWorkspaceID(req.Workspace)
	if err != nil {
		return ShortcutIterationUpsertResult{}, err
	}

	var iterationID int
	err = s.db.QueryRow(`
		SELECT iteration_id FROM shortcut_sync_iterations
		WHERE external_id = ? AND workspace_id = ?`, req.ExternalID, workspaceID).Scan(&iterationID)
	if err == nil {
		return ShortcutIterationUpsertResult{IterationID: iterationID}, nil
	}
	if err != sql.ErrNoRows {
		return ShortcutIterationUpsertResult{}, fmt.Errorf("shortcut sync: find iteration mapping %d: %w", req.ExternalID, err)
	}

	// Shortcut's own state first, falling back to the wall clock: Shortcut
	// iterations often stay "unfinished" forever after their end date.
	status := iterationStatus(req.Status, end)
	now := time.Now().UTC()
	var typeID int
	if err := s.db.QueryRow(`SELECT id FROM iteration_types WHERE name = 'Sprint' LIMIT 1`).Scan(&typeID); err == sql.ErrNoRows {
		// The Sprint type is seeded; a missing row still allows a NULL type.
		typeID = 0
	} else if err != nil {
		return ShortcutIterationUpsertResult{}, fmt.Errorf("shortcut sync: resolve iteration type: %w", err)
	}
	inserted := false
	err = database.WithTx(s.db, func(tx database.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO iterations (name, start_date, end_date, status, type_id, is_global, workspace_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, FALSE, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			name, start, end, status, nullableID(typeID), workspaceID, now, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		if inserted {
			id, idErr := res.LastInsertId()
			if idErr != nil {
				return idErr
			}
			iterationID = int(id)
			return nil
		}
		// Same (workspace, name) already exists: adopt it so concurrent
		// stories in the same sprint converge on one row.
		return tx.QueryRow(`
			SELECT id FROM iterations WHERE workspace_id = ? AND name = ? LIMIT 1`,
			workspaceID, name).Scan(&iterationID)
	})
	if err != nil {
		return ShortcutIterationUpsertResult{}, fmt.Errorf("shortcut sync: import iteration %d: %w", req.ExternalID, err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO shortcut_sync_iterations (external_id, workspace_id, iteration_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(external_id, workspace_id) DO NOTHING`,
		req.ExternalID, workspaceID, iterationID, now, now); err != nil {
		return ShortcutIterationUpsertResult{}, fmt.Errorf("shortcut sync: insert iteration mapping: %w", err)
	}
	return ShortcutIterationUpsertResult{IterationID: iterationID, Created: inserted}, nil
}

// normalizeIterationDates coerces Shortcut's date payloads into the two
// YYYY-MM-DD strings windshift iterations require. Story iteration windows
// are real date ranges; a missing or malformed range falls back to the day
// of the request rather than failing the story that references it.
func normalizeIterationDates(startRaw, endRaw string) (string, string, error) {
	start := strings.TrimSpace(startRaw)
	end := strings.TrimSpace(endRaw)
	day := func(raw string) (string, error) {
		for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02T15:04:05Z"} {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.UTC().Format("2006-01-02"), nil
			}
		}
		return "", fmt.Errorf("unparseable iteration date %q", raw)
	}
	if start != "" {
		if d, err := day(start); err == nil {
			start = d
		} else {
			start = ""
		}
	}
	if end != "" {
		if d, err := day(end); err == nil {
			end = d
		} else {
			end = ""
		}
	}
	if start == "" && end == "" {
		today := time.Now().UTC().Format("2006-01-02")
		return today, today, nil
	}
	if start == "" {
		start = end
	}
	if end == "" {
		end = start
	}
	if end < start {
		start, end = end, start
	}
	return start, end, nil
}

// iterationStatus picks the windshift iteration status from Shortcut's state
// first, then the dates: past-dated iterations import as completed even when
// Shortcut never closed them.
func iterationStatus(shortcutState string, end string) string {
	switch strings.ToLower(strings.TrimSpace(shortcutState)) {
	case "done", "completed":
		return "completed"
	case "started", "active":
		return "active"
	case "unstarted", "planned":
		return "planned"
	}
	today := time.Now().UTC().Format("2006-01-02")
	switch {
	case today > end:
		return "completed"
	case today == end:
		return "active"
	default:
		return "planned"
	}
}

// nullableID maps a 0 id to SQL NULL (optional FKs take integers only).
func nullableID(id int) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *ShortcutSyncService) resolveWorkspaceID(raw string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || id <= 0 {
		return 0, &validation.ValidationError{Field: "workspace_id", Message: "must be a decimal workspace id"}
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM workspaces WHERE id = ?", id).Scan(&count); err != nil {
		return 0, fmt.Errorf("shortcut sync: resolve workspace: %w", err)
	}
	if count == 0 {
		return 0, &validation.ValidationError{Field: "workspace_id", Message: "workspace not found"}
	}
	return id, nil
}

func parseShortcutTimestamp(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	utc := parsed.UTC()
	return &utc, nil
}

func externalUpdatedAtOrNil(raw string) any {
	parsed, err := parseShortcutTimestamp(raw)
	if err != nil || parsed == nil {
		return nil
	}
	return *parsed
}

// ShortcutWorkflowState is one state of a Shortcut team's workflow, in board
// order. Type is Shortcut's state type ("unstarted"|"started"|"done") and
// selects the windshift status category.
type ShortcutWorkflowState struct {
	Name string
	Type string
}

// ShortcutWorkspaceUpsertRequest is the workspace_upsert wire payload
// (shortcut-v1-contracts.md §1): find-or-create the target workspace for a
// Shortcut team or project. ExternalKind "" means "team" (payloads from
// before the per-project mode carried no kind).
type ShortcutWorkspaceUpsertRequest struct {
	ExternalID   int64                   // Shortcut team or project id
	ExternalKind string                  // "team" (default) | "project"
	Name         string                  // name, becomes the workspace name
	CreatorID    int                     // Windshift user granted workspace admin (0 = none, same as the Jira import path)
	States       []ShortcutWorkflowState // team workflow states in board order (kind "team" binds them as the workspace's workflow)
}

// ShortcutWorkspaceUpsertResult reports the resolved workspace.
type ShortcutWorkspaceUpsertResult struct {
	WorkspaceID int
	Created     bool // true when this call created the workspace
}

// UpsertWorkspace resolves a Shortcut team to its Windshift workspace,
// creating it on first use through the canonical WorkspaceService.Create
// path (defaults, key uniqueness, count-cache invalidation). Idempotent via
// shortcut_sync_workspaces; a crashed attempt that left a workspace without
// a mapping row is adopted by its deterministic key (SC-<teamid>).
func (s *ShortcutSyncService) UpsertWorkspace(ctx context.Context, req ShortcutWorkspaceUpsertRequest) (ShortcutWorkspaceUpsertResult, error) {
	if req.ExternalID <= 0 {
		return ShortcutWorkspaceUpsertResult{}, &validation.ValidationError{Field: "external_id", Message: "must be a positive Shortcut team or project id"}
	}
	if req.ExternalKind == "" {
		req.ExternalKind = "team"
	}
	if req.ExternalKind != "team" && req.ExternalKind != "project" {
		return ShortcutWorkspaceUpsertResult{}, &validation.ValidationError{Field: "external_kind", Message: "must be \"team\" or \"project\""}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ShortcutWorkspaceUpsertResult{}, &validation.ValidationError{Field: "name", Message: "name is required"}
	}

	if mapping, err := s.findWorkspaceMapping(req.ExternalKind, req.ExternalID); err != nil {
		return ShortcutWorkspaceUpsertResult{}, err
	} else if mapping != nil {
		if req.ExternalKind == "team" {
			// A workspace whose binding was interrupted before the config-set
			// row landed gets a second chance; already-bound ones no-op.
			if err := s.bindTeamWorkflow(mapping.workspaceID, name, req.States); err != nil {
				return ShortcutWorkspaceUpsertResult{}, err
			}
		}
		return ShortcutWorkspaceUpsertResult{WorkspaceID: mapping.workspaceID}, nil
	}

	created := false
	wsID, err := func() (int, error) {
		// Team keys keep the v0.3 format (adoption of pre-existing orphan
		// workspaces must keep working); project keys are namespaced.
		key := fmt.Sprintf("SC-%d", req.ExternalID)
		if req.ExternalKind == "project" {
			key = fmt.Sprintf("SC-project-%d", req.ExternalID)
		}
		var adopted int
		err := s.db.QueryRow("SELECT id FROM workspaces WHERE key = ?", key).Scan(&adopted)
		if err == nil {
			return adopted, nil // orphan from an interrupted prior attempt
		}
		if err != sql.ErrNoRows {
			return 0, fmt.Errorf("shortcut sync: find workspace by key: %w", err)
		}
		if runes := []rune(name); len(runes) > 80 {
			name = string(runes[:80])
		}
		result, err := NewWorkspaceService(s.db).Create(ctx, CreateWorkspaceParams{
			Name:        name,
			Key:         key,
			Description: fmt.Sprintf("Imported from Shortcut %s %d", req.ExternalKind, req.ExternalID),
			CreatorID:   req.CreatorID,
		})
		if err != nil {
			return 0, fmt.Errorf("shortcut sync: create workspace: %w", err)
		}
		created = true
		return result.Workspace.ID, nil
	}()
	if err != nil {
		return ShortcutWorkspaceUpsertResult{}, err
	}

	now := time.Now().UTC()
	if _, err := s.db.Exec(`INSERT INTO shortcut_sync_workspaces (external_kind, external_id, workspace_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, req.ExternalKind, req.ExternalID, wsID, now, now); err != nil {
		return ShortcutWorkspaceUpsertResult{}, fmt.Errorf("shortcut sync: insert workspace mapping: %w", err)
	}
	if req.ExternalKind == "team" {
		if err := s.bindTeamWorkflow(wsID, name, req.States); err != nil {
			return ShortcutWorkspaceUpsertResult{}, err
		}
	}
	return ShortcutWorkspaceUpsertResult{WorkspaceID: wsID, Created: created}, nil
}

// bindTeamWorkflow gives a team workspace the look of its Shortcut team: a
// workflow named after the team whose columns are the team's states, a
// configuration set wiring the two, and the workspace pinned to that set —
// the same chain the UI uses to render a board
// (WorkspaceService.GetStatuses → workspace_configuration_sets →
// configuration_sets.workflow_id → workflow_transitions). Statuses are global
// rows found-or-created by name with the category derived from Shortcut's
// state type, so two teams sharing a state name share one status. Idempotent:
// a workspace that already has a binding row is left untouched.
func (s *ShortcutSyncService) bindTeamWorkflow(wsID int, teamName string, states []ShortcutWorkflowState) error {
	if len(states) == 0 {
		return nil
	}
	var bound int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM workspace_configuration_sets WHERE workspace_id = ?", wsID).Scan(&bound); err != nil {
		return fmt.Errorf("shortcut sync: check workspace binding: %w", err)
	}
	if bound > 0 {
		return nil
	}

	categoryByType := map[string]string{"backlog": "to_do", "unstarted": "to_do", "started": "in_progress", "done": "done"}
	statusIDs := make([]int, 0, len(states))
	for _, st := range states {
		stateName := strings.TrimSpace(st.Name)
		if stateName == "" {
			continue
		}
		category := categoryByType[strings.ToLower(strings.TrimSpace(st.Type))]
		if category == "" {
			category = "in_progress"
		}
		id, err := s.findOrCreateStatusID(stateName, category)
		if err != nil {
			return err
		}
		statusIDs = append(statusIDs, id)
	}
	if len(statusIDs) == 0 {
		return nil
	}

	return database.WithTx(s.db, func(tx database.Tx) error {
		var workflowID int
		err := tx.QueryRow("SELECT id FROM workflows WHERE name = ? ORDER BY id LIMIT 1", teamName).Scan(&workflowID)
		if err == sql.ErrNoRows {
			res, insErr := tx.Exec("INSERT INTO workflows (name, created_at, updated_at) VALUES (?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)", teamName)
			if insErr != nil {
				return fmt.Errorf("shortcut sync: create workflow %q: %w", teamName, insErr)
			}
			id, idErr := res.LastInsertId()
			if idErr != nil {
				return fmt.Errorf("shortcut sync: workflow id: %w", idErr)
			}
			workflowID = int(id)
		} else if err != nil {
			return fmt.Errorf("shortcut sync: find workflow %q: %w", teamName, err)
		}
		// Columns: NULL → first, first → second, … Chain from every status
		// into the last one too so work can always reach Done.
		for i, to := range statusIDs {
			var from any
			if i > 0 {
				from = statusIDs[i-1]
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO workflow_transitions (workflow_id, from_status_id, to_status_id, from_all_statuses, display_order, created_at)
				VALUES (?, ?, ?, false, ?, CURRENT_TIMESTAMP)`, workflowID, from, to, i); err != nil {
				return fmt.Errorf("shortcut sync: workflow transition %d: %w", i, err)
			}
		}
		if len(statusIDs) > 1 {
			if _, err := tx.Exec("INSERT OR IGNORE INTO workflow_transitions (workflow_id, from_status_id, to_status_id, from_all_statuses, display_order, created_at) VALUES (?, NULL, ?, true, ?, CURRENT_TIMESTAMP)",
				workflowID, statusIDs[len(statusIDs)-1], len(statusIDs)); err != nil {
				return fmt.Errorf("shortcut sync: workflow all-status transition: %w", err)
			}
		}
		var configSetID int
		configSetName := "Shortcut: " + teamName
		err = tx.QueryRow("SELECT id FROM configuration_sets WHERE name = ? LIMIT 1", configSetName).Scan(&configSetID)
		if err == sql.ErrNoRows {
			res, insErr := tx.Exec("INSERT INTO configuration_sets (name, workflow_id, created_at, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)", configSetName, workflowID)
			if insErr != nil {
				return fmt.Errorf("shortcut sync: create configuration set %q: %w", configSetName, insErr)
			}
			id, idErr := res.LastInsertId()
			if idErr != nil {
				return fmt.Errorf("shortcut sync: configuration set id: %w", idErr)
			}
			configSetID = int(id)
		} else if err != nil {
			return fmt.Errorf("shortcut sync: find configuration set %q: %w", configSetName, err)
		}
		if _, err := tx.Exec("INSERT INTO workspace_configuration_sets (workspace_id, configuration_set_id, created_at) VALUES (?, ?, CURRENT_TIMESTAMP)", wsID, configSetID); err != nil {
			return fmt.Errorf("shortcut sync: bind workspace %d to configuration set: %w", wsID, err)
		}
		return nil
	})
}

// findOrCreateStatusID returns the id of the global status with this name,
// creating it under the given status category when missing.
func (s *ShortcutSyncService) findOrCreateStatusID(name, categoryBuiltinKey string) (int, error) {
	var id int
	err := s.db.QueryRow("SELECT id FROM statuses WHERE name = ? LIMIT 1", name).Scan(&id)
	if err == nil {
		// An existing status may carry a stale category (bound under an
		// earlier type mapping); align it so the board group matches.
		if _, err := s.db.Exec(`UPDATE statuses SET category_id = (SELECT id FROM status_categories WHERE builtin_key = ? LIMIT 1), updated_at = CURRENT_TIMESTAMP WHERE id = ? AND category_id != (SELECT id FROM status_categories WHERE builtin_key = ? LIMIT 1)`, categoryBuiltinKey, id, categoryBuiltinKey); err != nil {
			return 0, fmt.Errorf("shortcut sync: align status %q category: %w", name, err)
		}
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("shortcut sync: find status %q: %w", name, err)
	}
	var categoryID int
	if err := s.db.QueryRow("SELECT id FROM status_categories WHERE builtin_key = ? LIMIT 1", categoryBuiltinKey).Scan(&categoryID); err != nil {
		return 0, fmt.Errorf("shortcut sync: find status category %q: %w", categoryBuiltinKey, err)
	}
	if err := s.db.QueryRow("INSERT INTO statuses (name, category_id, is_default, created_at, updated_at) VALUES (?, ?, false, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING id", name, categoryID).Scan(&id); err != nil {
		// Lost a create race (or the UNIQUE name fired): read the winner.
		if retryErr := s.db.QueryRow("SELECT id FROM statuses WHERE name = ? LIMIT 1", name).Scan(&id); retryErr != nil {
			return 0, fmt.Errorf("shortcut sync: create status %q: %w", name, err)
		}
	}
	return id, nil
}

// PurgeSyncResult reports what the purge removed.
type PurgeSyncResult struct {
	Items             int   `json:"items"`
	KVEntries         int64 `json:"kv_entries"`
	Workspaces        int   `json:"workspaces"`
	Statuses          int   `json:"statuses"`
	Workflows         int   `json:"workflows"`
	ConfigurationSets int   `json:"configuration_sets"`
}

// PurgeSync deletes every artifact the sync plugin ever created: item mappings
// (their items cascade), the plugin's KV state (sync cursors, comment dedup,
// tombstones), plugin-created workspaces, the team workflows/configuration
// sets the plugin bound, and the non-builtin statuses it minted. The
// operator's fallback workspace is preserved even though it carries a mapping
// — it predates the sync and holds non-plugin items. Items go through the
// item repository's deletion path so relations outside the FK cascade
// (watches, history, links, worklogs) are cleaned the same way the UI delete
// uses.
func (s *ShortcutSyncService) PurgeSync(ctx context.Context, pluginName string) (PurgeSyncResult, error) {
	var result PurgeSyncResult

	err := database.WithTx(s.db, func(tx database.Tx) error {
		// Mapped items: delete the sync rows first so a later pass sees them
		// gone, then the items themselves.
		rows, err := tx.Query(`SELECT item_id FROM shortcut_sync_items`)
		if err != nil {
			return fmt.Errorf("shortcut sync: list mapped items: %w", err)
		}
		var itemIDs []int
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			itemIDs = append(itemIDs, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()

		repo := repository.NewItemRepository(s.db)
		for _, itemID := range itemIDs {
			if err := repo.DeleteItemWatches(tx, itemID); err != nil {
				return err
			}
			if err := repo.DeleteItemHistory(tx, itemID); err != nil {
				return err
			}
			if err := repo.DeleteItemLinks(tx, itemID); err != nil {
				return err
			}
			if err := repo.ClearWorklogItemReferences(tx, itemID); err != nil {
				return err
			}
			if err := repo.Delete(tx, itemID); err != nil {
				return err
			}
		}
		result.Items = len(itemIDs)

		if _, err := tx.Exec(`DELETE FROM shortcut_sync_items`); err != nil {
			return fmt.Errorf("shortcut sync: clear item mappings: %w", err)
		}
		// Imported iterations: workspace-cascaded rows are gone already;
		// fallback-workspace ones (NS survives the purge) go via the mapping
		// list before it clears.
		if _, err := tx.Exec(`DELETE FROM iterations WHERE id IN (SELECT iteration_id FROM shortcut_sync_iterations)`); err != nil {
			return fmt.Errorf("shortcut sync: delete imported iterations: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM shortcut_sync_users`); err != nil {
			return fmt.Errorf("shortcut sync: clear user mappings: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM shortcut_sync_iterations`); err != nil {
			return fmt.Errorf("shortcut sync: clear iteration mappings: %w", err)
		}
		// Imported users carry the synthetic sync email and own nothing but
		// their assignee slots, so a hard delete is safe and makes a
		// re-import derive them fresh.
		if _, err := tx.Exec(`DELETE FROM users WHERE email LIKE 'sync-member-%@shortcut.import'`); err != nil {
			return fmt.Errorf("shortcut sync: delete imported users: %w", err)
		}
		// The plugin's KV: sync cursors, tombstones, comment dedup map. The
		// config survives (operator settings; the replay reuses it).
		res, err := tx.Exec(`
			DELETE FROM plugin_kv_store
			WHERE plugin_name = ?
			  AND (key = 'shortcut:state' OR key LIKE 'shortcut:cmt:%' OR key LIKE 'shortcut:tomb:%')`,
			pluginName)
		if err != nil {
			return fmt.Errorf("shortcut sync: clear plugin kv: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.KVEntries = n
		}

		// Workspaces created by the sync — the fallback workspace keeps its
		// mapping and its rows (it may hold non-plugin items).
		rows, err = tx.Query(`
			SELECT w.id FROM shortcut_sync_workspaces w
			JOIN workspaces ws ON ws.id = w.workspace_id
			WHERE ws.key LIKE 'SC-%'`)
		if err != nil {
			return fmt.Errorf("shortcut sync: list plugin workspaces: %w", err)
		}
		var wsIDs []int
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			wsIDs = append(wsIDs, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		for _, wsID := range wsIDs {
			if _, err := tx.Exec("DELETE FROM workspaces WHERE id = ?", wsID); err != nil {
				return fmt.Errorf("shortcut sync: delete workspace %d: %w", wsID, err)
			}
		}
		result.Workspaces = len(wsIDs)
		if _, err := tx.Exec(`DELETE FROM shortcut_sync_workspaces`); err != nil {
			return fmt.Errorf("shortcut sync: clear workspace mappings: %w", err)
		}

		// The plugin's configuration sets. Workspace deletions above cascaded
		// their binding rows away, so anything still named "Shortcut: …" and
		// unbound is plugin-owned.
		res, err = tx.Exec(`
			DELETE FROM configuration_sets
			WHERE name LIKE 'Shortcut: %'
			  AND id NOT IN (SELECT configuration_set_id FROM workspace_configuration_sets)`)
		if err != nil {
			return fmt.Errorf("shortcut sync: delete configuration sets: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.ConfigurationSets = int(n)
		}

		// Team workflows: after the config-set deletion nothing references
		// them (items carry statuses, not workflows), and their transitions
		// cascade. builtin_key IS NULL keeps the default catalog intact.
		res, err = tx.Exec(`
			DELETE FROM workflows
			WHERE builtin_key IS NULL
			  AND id NOT IN (SELECT workflow_id FROM configuration_sets WHERE workflow_id IS NOT NULL)`)
		if err != nil {
			return fmt.Errorf("shortcut sync: delete team workflows: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.Workflows = int(n)
		}

		// Non-builtin statuses no item or transition references — the names
		// the plugin minted beyond the default catalog.
		res, err = tx.Exec(`
			DELETE FROM statuses
			WHERE builtin_key IS NULL
			  AND id NOT IN (SELECT status_id FROM items WHERE status_id IS NOT NULL)
			  AND id NOT IN (SELECT from_status_id FROM workflow_transitions WHERE from_status_id IS NOT NULL)
			  AND id NOT IN (SELECT to_status_id FROM workflow_transitions)`)
		if err != nil {
			return fmt.Errorf("shortcut sync: delete orphan statuses: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.Statuses = int(n)
		}
		return nil
	})
	if err != nil {
		return PurgeSyncResult{}, err
	}
	return result, nil
}

func (s *ShortcutSyncService) findWorkspaceMapping(externalKind string, externalID int64) (*shortcutWorkspaceMapping, error) {
	var m shortcutWorkspaceMapping
	err := s.db.QueryRow(`
		SELECT workspace_id FROM shortcut_sync_workspaces
		WHERE external_kind = ? AND external_id = ?`, externalKind, externalID).Scan(&m.workspaceID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shortcut sync: find workspace mapping: %w", err)
	}
	return &m, nil
}

type shortcutWorkspaceMapping struct {
	workspaceID int
}
