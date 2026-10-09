package services

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"windshift/internal/database"
	"windshift/internal/repository"
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

func TestShortcutSyncWorkspaceUpsertCreatesAndIsIdempotent(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	res, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 42, Name: "Platform Team"})
	if err != nil {
		t.Fatalf("workspace upsert: %v", err)
	}
	if !res.Created {
		t.Fatalf("expected Created=true on first upsert")
	}

	var name, key, description string
	if err := db.QueryRow(`SELECT name, key, description FROM workspaces WHERE id = ?`, res.WorkspaceID).Scan(&name, &key, &description); err != nil {
		t.Fatal(err)
	}
	if name != "Platform Team" || key != "SC-42" {
		t.Fatalf("unexpected workspace %q key %q", name, key)
	}

	// Idempotent: same mapping, no second workspace.
	again, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 42, Name: "Renamed Team"})
	if err != nil {
		t.Fatalf("workspace re-upsert: %v", err)
	}
	if again.Created || again.WorkspaceID != res.WorkspaceID {
		t.Fatalf("expected same workspace without creation, got %+v", again)
	}
	if err := db.QueryRow(`SELECT name FROM workspaces WHERE id = ?`, res.WorkspaceID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Platform Team" {
		t.Fatalf("mapping hit must not rename workspace, got %q", name)
	}
}

// TestShortcutSyncWorkspaceUpsertBindsTeamWorkflow covers the team-mode
// binding: states ride the upsert, the core mints the workflow/statuses/
// configuration set and pins the workspace to it. The host-function
// passthrough (req.States → service) once dropped the field silently, so
// this asserts the full chain, not just the upsert response.
func TestShortcutSyncWorkspaceUpsertBindsTeamWorkflow(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	states := []ShortcutWorkflowState{
		{Name: "Backlog", Type: "backlog"},
		{Name: "In Development", Type: "started"},
		{Name: "Completed", Type: "done"},
	}
	res, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{
		ExternalID: 77, Name: "Tech Development", States: states,
	})
	if err != nil {
		t.Fatalf("team workspace upsert: %v", err)
	}

	var configSetID int
	if err := db.QueryRow(`SELECT configuration_set_id FROM workspace_configuration_sets WHERE workspace_id = ?`, res.WorkspaceID).Scan(&configSetID); err != nil {
		t.Fatalf("workspace binding missing: %v", err)
	}

	var workflowID int
	var configName string
	if err := db.QueryRow(`SELECT workflow_id, name FROM configuration_sets WHERE id = ?`, configSetID).Scan(&workflowID, &configName); err != nil {
		t.Fatalf("configuration set missing: %v", err)
	}
	if configName != "Shortcut: Tech Development" {
		t.Fatalf("configuration set name = %q", configName)
	}

	// Board columns = the workflow's reachable statuses: the binder chains
	// NULL→s0→…→sN and adds an all-statuses→last edge. Every input state must
	// appear as a transition target exactly once.
	var columnCount int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT to_status_id) FROM workflow_transitions WHERE workflow_id = ?`, workflowID).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != len(states) {
		t.Fatalf("workflow columns = %d, want %d", columnCount, len(states))
	}
	var inDevCategory string
	if err := db.QueryRow(`
		SELECT c.builtin_key FROM statuses s
		JOIN status_categories c ON c.id = s.category_id
		WHERE s.name = 'In Development' LIMIT 1`).Scan(&inDevCategory); err != nil {
		t.Fatalf("status 'In Development' missing: %v", err)
	}
	if inDevCategory != "in_progress" {
		t.Fatalf("'In Development' category = %q, want in_progress", inDevCategory)
	}

	// Re-upsert with states must not duplicate the set: the binder
	// short-circuits on an existing workspace binding (NULL rows stay at
	// two: the chain head plus the all-statuses edge).
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_transitions WHERE workflow_id = ? AND from_status_id IS NULL`, workflowID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{
		ExternalID: 77, Name: "Tech Development", States: states,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_transitions WHERE workflow_id = ? AND from_status_id IS NULL`, workflowID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("re-upsert duplicated transitions: %d → %d", before, after)
	}
}

func TestShortcutSyncWorkspaceUpsertKindIsolation(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	team, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 42, Name: "Platform Team"})
	if err != nil {
		t.Fatalf("team upsert: %v", err)
	}
	// Same numeric id under "project" must NOT resolve to the team mapping.
	project, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 42, ExternalKind: "project", Name: "Aviator"})
	if err != nil {
		t.Fatalf("project upsert: %v", err)
	}
	if project.WorkspaceID == team.WorkspaceID {
		t.Fatalf("project id 42 collided with team mapping: both ws=%d", team.WorkspaceID)
	}
	if !project.Created {
		t.Fatalf("project upsert should create its own workspace, got Created=false ws=%d", project.WorkspaceID)
	}
	replay, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 42, Name: "Platform Team"})
	if err != nil {
		t.Fatalf("team replay: %v", err)
	}
	if replay.Created || replay.WorkspaceID != team.WorkspaceID {
		t.Fatalf("team replay drifted: created=%v ws=%d want ws=%d", replay.Created, replay.WorkspaceID, team.WorkspaceID)
	}
	var key string
	if err := db.QueryRow(`SELECT key FROM workspaces WHERE id = ?`, project.WorkspaceID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	// WorkspaceService.Create normalizes keys to upper case.
	if key != "SC-PROJECT-42" {
		t.Fatalf("project workspace key %q, want SC-PROJECT-42", key)
	}

	if _, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 1, ExternalKind: "milestone", Name: "x"}); err == nil {
		t.Fatalf("expected validation error for unknown external_kind")
	}
}

func TestShortcutSyncWorkspaceUpsertAdoptsOrphanWorkspace(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()

	// Orphan from an interrupted prior attempt: workspace exists, no mapping.
	var orphanID int
	if err := db.QueryRow(`INSERT INTO workspaces (name, key) VALUES ('Interrupted', 'SC-77') RETURNING id`).Scan(&orphanID); err != nil {
		t.Fatal(err)
	}

	res, err := service.UpsertWorkspace(context.Background(), ShortcutWorkspaceUpsertRequest{ExternalID: 77, Name: "Interrupted"})
	if err != nil {
		t.Fatalf("workspace upsert: %v", err)
	}
	if res.Created || res.WorkspaceID != orphanID {
		t.Fatalf("expected adoption of orphan without creation, got %+v", res)
	}

	var mappingID int
	if err := db.QueryRow(`SELECT workspace_id FROM shortcut_sync_workspaces WHERE external_kind = 'team' AND external_id = 77`).Scan(&mappingID); err != nil {
		t.Fatal(err)
	}
	if mappingID != orphanID {
		t.Fatalf("mapping points at %d, want orphan %d", mappingID, orphanID)
	}
}

func TestShortcutSyncWorkspaceUpsertValidatesInput(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if _, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 0, Name: "x"}); err == nil {
		t.Fatalf("expected validation error for zero external id")
	}
	if _, err := service.UpsertWorkspace(ctx, ShortcutWorkspaceUpsertRequest{ExternalID: 5, Name: "  "}); err == nil {
		t.Fatalf("expected validation error for blank name")
	}
}

// TestShortcutSyncUpsertUpdateRebindsParent covers the update path: it builds
// an any-typed payload map, and a leaked *int in it fails the update
// validator's CoerceInt as "Invalid parent_id type" (seen live on story 78298
// → item 4695 during the 2026-10-07 backfill). Re-binding the same parent on
// an existing item must accept the dereferenced id.
func TestShortcutSyncUpsertUpdateRebindsParent(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, _, _ := seedShortcutSyncFixture(t, db)
	ctx := context.Background()

	// Parentable type pair: child level must be exactly parent level + 1.
	if _, err := db.Exec(`INSERT INTO item_types (name, is_default, hierarchy_level) VALUES ('SS-Epic', false, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO item_types (name, is_default, hierarchy_level) VALUES ('SS-Story', true, 2)`); err != nil {
		t.Fatal(err)
	}

	epicRes, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID:  strconv.Itoa(workspaceID),
		ExternalKind: "epic",
		ExternalID:   99001,
		Title:        "Imported epic",
		ItemTypeName: "SS-Epic",
	})
	if err != nil {
		t.Fatalf("epic create: %v", err)
	}

	res, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID:        strconv.Itoa(workspaceID),
		ExternalKind:       "story",
		ExternalID:         99002,
		Title:              "Imported story",
		ItemTypeName:       "SS-Story",
		ParentExternalKind: "epic",
		ParentExternalID:   99001,
	})
	if err != nil {
		t.Fatalf("story create with parent: %v", err)
	}

	res2, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID:        strconv.Itoa(workspaceID),
		ExternalKind:       "story",
		ExternalID:         99002,
		Title:              "Renamed under epic",
		ItemTypeName:       "SS-Story",
		ParentExternalKind: "epic",
		ParentExternalID:   99001,
	})
	if err != nil {
		t.Fatalf("story update with parent: %v", err)
	}
	if res2.Created || res2.ItemID != res.ItemID {
		t.Fatalf("unexpected update result: %+v (first %+v)", res2, res)
	}

	var parentID *int
	if err := db.QueryRow(`SELECT parent_id FROM items WHERE id = ?`, res.ItemID).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	if parentID == nil || *parentID != epicRes.ItemID {
		t.Fatalf("parent_id = %v, want %d", parentID, epicRes.ItemID)
	}
}

// TestShortcutSyncMemberAndIterationUpserts covers the on-demand assignee
// and sprint imports: member → user mapping (synthetic email when Shortcut
// omits the address), iteration → workspace-local sprint, and the story
// wiring — item_upsert resolves both mappings onto items.assignee_id /
// items.iteration_id on create and update.
func TestShortcutSyncMemberAndIterationUpserts(t *testing.T) {
	service, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, _, _ := seedShortcutSyncFixture(t, db)
	ctx := context.Background()

	// Member → user. Re-upsert is idempotent.
	mem, err := service.UpsertMember(ctx, ShortcutMemberUpsertRequest{
		ExternalID: "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8", Name: "Dev Yuri", Email: "yuri@nsx.dev", UsernameHint: "dev.yuri",
	})
	if err != nil {
		t.Fatalf("member upsert: %v", err)
	}
	var firstName string
	if err := db.QueryRow(`SELECT first_name FROM users WHERE id = ?`, mem.UserID).Scan(&firstName); err != nil {
		t.Fatalf("imported user missing: %v", err)
	}
	if firstName != "Dev" {
		t.Fatalf("first_name = %q, want Dev", firstName)
	}
	if again, err := service.UpsertMember(ctx, ShortcutMemberUpsertRequest{ExternalID: "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8", Name: "Dev Yuri"}); err != nil || again.UserID != mem.UserID {
		t.Fatalf("member re-upsert: %v (id %d vs %d)", err, again.UserID, mem.UserID)
	}

	// Iteration → workspace-local sprint.
	it, err := service.UpsertIteration(ctx, ShortcutIterationUpsertRequest{
		ExternalID: 4242, Name: "Sprint 42", StartDate: "2026-10-06", EndDate: "2026-10-20",
		Status: "unstarted", Workspace: strconv.Itoa(workspaceID),
	})
	if err != nil {
		t.Fatalf("iteration upsert: %v", err)
	}
	var itStatus string
	var itWorkspace int
	if err := db.QueryRow(`SELECT status, workspace_id FROM iterations WHERE id = ?`, it.IterationID).Scan(&itStatus, &itWorkspace); err != nil {
		t.Fatalf("imported iteration missing: %v", err)
	}
	if itStatus != "planned" {
		t.Fatalf("iteration status = %q, want planned (Shortcut unstarted)", itStatus)
	}
	if itWorkspace != workspaceID {
		t.Fatalf("iteration workspace = %d, want %d", itWorkspace, workspaceID)
	}
	if again, err := service.UpsertIteration(ctx, ShortcutIterationUpsertRequest{
		ExternalID: 4242, Name: "Sprint 42", Workspace: strconv.Itoa(workspaceID),
	}); err != nil || again.IterationID != it.IterationID {
		t.Fatalf("iteration re-upsert: %v (id %d vs %d)", err, again.IterationID, it.IterationID)
	}

	// Story wiring: assignee + iteration land on the item.
	res, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(workspaceID), ExternalKind: "story", ExternalID: 42042,
		Title: "Launch chain story", StatusName: "To Do", ItemTypeName: "Story",
		AssigneeExternalID: "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8", IterationExternalID: 4242,
	})
	if err != nil {
		t.Fatalf("story upsert: %v", err)
	}
	var assigneeID, iterationID any
	if err := db.QueryRow(`SELECT assignee_id, iteration_id FROM items WHERE id = ?`, res.ItemID).Scan(&assigneeID, &iterationID); err != nil {
		t.Fatal(err)
	}
	if assigneeID != int64(mem.UserID) && fmt.Sprint(assigneeID) != strconv.Itoa(mem.UserID) {
		t.Fatalf("assignee_id = %v, want %d", assigneeID, mem.UserID)
	}
	if fmt.Sprint(iterationID) != strconv.Itoa(it.IterationID) {
		t.Fatalf("iteration_id = %v, want %d", iterationID, it.IterationID)
	}

	// Update path: changing the iteration moves the item.
	it2, err := service.UpsertIteration(ctx, ShortcutIterationUpsertRequest{
		ExternalID: 4243, Name: "Sprint 43", StartDate: "2026-10-21", EndDate: "2026-11-04",
		Workspace: strconv.Itoa(workspaceID),
	})
	if err != nil {
		t.Fatalf("iteration 43: %v", err)
	}
	if _, err := service.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(workspaceID), ExternalKind: "story", ExternalID: 42042,
		Title: "Launch chain story", StatusName: "To Do", ItemTypeName: "Story",
		AssigneeExternalID: "687a7ce8-ae4b-4528-975d-bfe3fdc8bca8", IterationExternalID: 4243,
	}); err != nil {
		t.Fatalf("story update: %v", err)
	}
	if err := db.QueryRow(`SELECT iteration_id FROM items WHERE id = ?`, res.ItemID).Scan(&iterationID); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(iterationID) != strconv.Itoa(it2.IterationID) {
		t.Fatalf("iteration_id after update = %v, want %d", iterationID, it2.IterationID)
	}
}

// stubSyncPermChecker is a minimal WorkspacePermissionChecker so the
// cross-workspace tests can authorize or deny without a permission cache.
type stubSyncPermChecker struct{ allow bool }

func (c stubSyncPermChecker) HasWorkspacePermission(int, int, string) (bool, error) {
	return c.allow, nil
}

func (c stubSyncPermChecker) AccessibleWorkspaceIDs(int) ([]int, error) { return nil, nil }

func (c stubSyncPermChecker) AccessibleWorkspaceIDKeys(int) ([]repository.IDKey, error) { return nil, nil }

// TestShortcutSyncCrossWorkspaceParentAuthorizes pins the team-mode parent
// rule: a story update that attaches an epic living in another workspace
// fails closed without a checker and succeeds through an authorized one.
func TestShortcutSyncCrossWorkspaceParentAuthorizes(t *testing.T) {
	ctx := context.Background()
	_, db := newShortcutSyncTestService(t)
	defer func() { _ = db.Close() }()
	workspaceID, _, _ := seedShortcutSyncFixture(t, db)

	var epicWsID int
	if err := db.QueryRow(`INSERT INTO workspaces (name, key) VALUES ('Sync Epics', 'SE') RETURNING id`).Scan(&epicWsID); err != nil {
		t.Fatal(err)
	}

	epicSvc := NewShortcutSyncService(db).WithPermissionChecker(stubSyncPermChecker{allow: true})
	epicRes, err := epicSvc.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(epicWsID), ExternalKind: "epic", ExternalID: 99101, Title: "Cross epic",
		ItemTypeName: "Epic",
	})
	if err != nil {
		t.Fatalf("epic create: %v", err)
	}

	storySvc := NewShortcutSyncService(db).WithPermissionChecker(nil)
	storyRes, err := storySvc.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(workspaceID), ExternalKind: "story", ExternalID: 99102, Title: "Cross story",
		ItemTypeName: "Story",
	})
	if err != nil {
		t.Fatalf("story create: %v", err)
	}

	// Denied checker: fail closed even with a checker wired.
	denied := NewShortcutSyncService(db).WithPermissionChecker(stubSyncPermChecker{allow: false})
	if _, err := denied.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(workspaceID), ExternalKind: "story", ExternalID: 99102, Title: "Cross story",
		ItemTypeName:  "Story",
		ParentExternalKind: "epic", ParentExternalID: 99101,
	}); err == nil {
		t.Fatal("cross-workspace parent update with a denying checker: want error, got nil")
	}

	// No checker: the update still fails closed (permChecker == nil).
	noChecker := NewShortcutSyncService(db).WithPermissionChecker(nil)
	if _, err := noChecker.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(workspaceID), ExternalKind: "story", ExternalID: 99102, Title: "Cross story",
		ItemTypeName:  "Story",
		ParentExternalKind: "epic", ParentExternalID: 99101,
	}); err == nil {
		t.Fatal("cross-workspace parent update without a checker: want error, got nil")
	}

	// Allowing checker: the parent binds.
	allowed := NewShortcutSyncService(db).WithPermissionChecker(stubSyncPermChecker{allow: true})
	if _, err := allowed.Upsert(ctx, ShortcutItemUpsertRequest{
		WorkspaceID: strconv.Itoa(workspaceID), ExternalKind: "story", ExternalID: 99102, Title: "Cross story",
		ItemTypeName:  "Story",
		ParentExternalKind: "epic", ParentExternalID: 99101,
	}); err != nil {
		t.Fatalf("cross-workspace parent update with an allowing checker: %v", err)
	}
	var parentID *int
	if err := db.QueryRow(`SELECT parent_id FROM items WHERE id = ?`, storyRes.ItemID).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	if parentID == nil || *parentID != epicRes.ItemID {
		t.Fatalf("parent_id = %v, want %d", parentID, epicRes.ItemID)
	}
}
