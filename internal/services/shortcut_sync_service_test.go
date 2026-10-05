package services

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"windshift/internal/database"
)

func newShortcutSyncTestService(t *testing.T) (*ShortcutSyncService, database.Database) {
	t.Helper()
	db, err := database.NewSQLiteDBWithPoolSizes(filepath.Join(t.TempDir(), "windshift.db"), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Initialize(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return NewShortcutSyncService(db), db
}

func seedShortcutSyncFixture(t *testing.T, db database.Database) (workspaceID, statusID, itemTypeID int) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO status_categories (name, color, is_default) VALUES ('In Progress', '#3b82f6', true)`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO statuses (name, category_id, is_default) VALUES ('To Do', 1, true) RETURNING id`).Scan(&statusID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO item_types (name, is_default, hierarchy_level) VALUES ('Story', true, 3)`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM item_types WHERE name = 'Story'`).Scan(&itemTypeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO item_types (name, is_default, hierarchy_level) VALUES ('Epic', false, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO workspaces (name, key) VALUES ('Shortcut Sync', 'SS') RETURNING id`).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	// Cover the seeded status in the workspace's default workflow.
	var workflowID int
	if err := db.QueryRow(`
		SELECT id FROM workflows
		ORDER BY (is_default = true) DESC, id
		LIMIT 1
	`).Scan(&workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_transitions (workflow_id, from_status_id, to_status_id, from_all_statuses) VALUES (?, NULL, ?, true)`, workflowID, statusID); err != nil {
		t.Fatal(err)
	}
	return workspaceID, statusID, itemTypeID
}

func TestShortcutSyncUpsertCreatesAndUpdates(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, _, itemTypeID := seedShortcutSyncFixture(t, db)
	ctx := context.Background()

	// Create path.
	res, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID:       strconv.Itoa(workspaceID),
		ExternalKind:      "story",
		ExternalID:        42001,
		Title:             "Imported story",
		Description:       "Imported description",
		StatusName:        "To Do",
		PriorityName:      "High",
		ItemTypeName:      "Story",
		Labels:            []string{"backend", "import"},
		ExternalURL:       "https://app.shortcut.com/story/42001",
		ExternalUpdatedAt: "2026-10-05T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("create upsert: %v", err)
	}
	if !res.Created || res.ItemID <= 0 || res.ItemKey == "" {
		t.Fatalf("unexpected create result: %+v", res)
	}
	if want := "SS-1"; res.ItemKey != want {
		t.Fatalf("item key = %q, want %q", res.ItemKey, want)
	}

	// Idempotent second upsert with same external id updates, not creates.
	res2, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID:       strconv.Itoa(workspaceID),
		ExternalKind:      "story",
		ExternalID:        42001,
		Title:             "Renamed story",
		StatusName:        "To Do",
		Labels:            []string{"backend"},
		ExternalUpdatedAt: "2026-10-05T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	if res2.Created || res2.ItemID != res.ItemID {
		t.Fatalf("unexpected update result: %+v (first %+v)", res2, res)
	}

	var title string
	var storyPoints *float64
	if err := db.QueryRow(`SELECT title, story_points FROM items WHERE id = ?`, res.ItemID).
		Scan(&title, &storyPoints); err != nil {
		t.Fatal(err)
	}
	if title != "Renamed story" {
		t.Fatalf("title = %q", title)
	}

	// Mapping row reflects the latest sync.
	var externalUpdatedAt time.Time
	var lastSyncedAt time.Time
	var mappingItemID int
	if err := db.QueryRow(`
		SELECT item_id, external_updated_at, last_synced_at
		FROM shortcut_sync_items
		WHERE external_kind = 'story' AND external_id = 42001
	`).Scan(&mappingItemID, &externalUpdatedAt, &lastSyncedAt); err != nil {
		t.Fatalf("mapping row: %v", err)
	}
	if mappingItemID != res.ItemID {
		t.Fatalf("mapping item_id = %d, want %d", mappingItemID, res.ItemID)
	}
	if got := externalUpdatedAt.UTC().Format(time.RFC3339); got != "2026-10-05T12:00:00Z" {
		t.Fatalf("external_updated_at = %q", got)
	}

	// Item type was honored on create.
	var createdTypeID int
	if err := db.QueryRow(`SELECT item_type_id FROM items WHERE id = ?`, res.ItemID).Scan(&createdTypeID); err != nil {
		t.Fatal(err)
	}
	if createdTypeID != itemTypeID {
		t.Fatalf("item_type_id = %d, want %d", createdTypeID, itemTypeID)
	}
}

func TestShortcutSyncLookupReturnsMapping(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, _, _ := seedShortcutSyncFixture(t, db)
	ctx := context.Background()

	res, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID:       strconv.Itoa(workspaceID),
		ExternalKind:      "epic",
		ExternalID:        7,
		Title:             "Imported epic",
		ItemTypeName:      "Epic",
		ExternalUpdatedAt: "2026-10-05T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	found, err := service.Lookup(ctx, "epic", 7)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found == nil || found.ItemID != res.ItemID || found.ItemKey != res.ItemKey {
		t.Fatalf("unexpected lookup result: %+v (upsert %+v)", found, res)
	}
	if found.ExternalUpdatedAt == nil || found.ExternalUpdatedAt.UTC().Format(time.RFC3339) != "2026-10-05T00:00:00Z" {
		t.Fatalf("lookup external_updated_at = %v", found.ExternalUpdatedAt)
	}
	if found.LastSyncedAt == nil {
		t.Fatal("lookup last_synced_at empty")
	}

	missing, err := service.Lookup(ctx, "story", 999999)
	if err != nil {
		t.Fatalf("missing lookup: %v", err)
	}
	if missing != nil {
		t.Fatalf("unexpected hit: %+v", missing)
	}
}

func TestShortcutSyncUpsertRejectsUnknownStatus(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, _, _ := seedShortcutSyncFixture(t, db)

	_, err := service.Upsert(context.Background(), ShortcutItemUpsertRequest{
		WorkspaceID:  strconv.Itoa(workspaceID),
		ExternalKind: "story",
		ExternalID:   42,
		Title:        "Broken status",
		StatusName:   "Does Not Exist",
	})
	if err == nil {
		t.Fatal("expected error for unknown status name")
	}
}

func TestShortcutSyncMergeLabelModePreservesExistingLabels(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, statusID, _ := seedShortcutSyncFixture(t, db)

	if _, err := db.Exec(`INSERT INTO labels (name) VALUES ('Core')`); err != nil {
		t.Fatal(err)
	}

	// Create with one label, then upsert again in merge mode with another.
	res, err := service.Upsert(context.Background(), ShortcutItemUpsertRequest{
		WorkspaceID:       strconv.Itoa(workspaceID),
		ExternalKind:      "story",
		ExternalID:        88,
		Title:             "Merge labels",
		StatusName:        "To Do",
		ItemTypeName:      "Story",
		Labels:            []string{"Core"},
		ExternalUpdatedAt: "2026-10-05T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("create upsert: %v", err)
	}

	res, err = service.Upsert(context.Background(), ShortcutItemUpsertRequest{
		WorkspaceID:       strconv.Itoa(workspaceID),
		ExternalKind:      "story",
		ExternalID:        88,
		Title:             "Merge labels",
		StatusName:        "To Do",
		ItemTypeName:      "Story",
		Labels:            []string{"Imported"},
		LabelMode:         "merge",
		ExternalUpdatedAt: "2026-10-05T00:01:00Z",
	})
	if err != nil {
		t.Fatalf("merge upsert: %v", err)
	}
	_ = statusID

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM item_labels WHERE item_id = ?`, res.ItemID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 labels after merge, got %d", count)
	}
}
