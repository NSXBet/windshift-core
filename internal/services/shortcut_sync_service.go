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
		updateData["parent_id"] = parentID
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
