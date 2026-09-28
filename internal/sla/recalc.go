package sla

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"windshift/internal/database"
	"windshift/internal/itemevents"
	"windshift/internal/models"
)

// DefaultRecalcPageSize is the number of items reconciled per transaction.
const DefaultRecalcPageSize = 200

// RecalculateItem reconciles one item's SLA cycles with its current state.
func (e *Engine) RecalculateItem(ctx context.Context, itemID int) error {
	return database.WithTx(e.db, func(tx database.Tx) error {
		return e.recalcItemTx(ctx, tx, itemID)
	})
}

// EnqueueRecalculation arms a recalc_metric job due now.
func (e *Engine) EnqueueRecalculation(ctx context.Context, metricID int) error {
	return database.WithTx(e.db, func(tx database.Tx) error {
		metric := metricID
		return e.repo.UpsertJob(ctx, tx, &models.SLAJob{Kind: models.SLAJobRecalcMetric, MetricID: &metric, DueAt: e.clock.Now()})
	})
}

// BumpConfigGeneration advances a workspace's configuration generation in the
// caller's transaction and returns the new value.
func (e *Engine) BumpConfigGeneration(ctx context.Context, tx database.Tx, workspaceID int) (int, error) {
	generation, err := e.repo.TouchWorkspaceState(ctx, tx, workspaceID)
	if err == nil {
		e.InvalidateWorkspace(workspaceID)
	}
	return generation, err
}

func (e *Engine) recalcItemTx(ctx context.Context, tx database.Tx, itemID int) error {
	snapshot, err := loadItemSnapshotTx(ctx, tx, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if snapshot.WorkspaceID == 0 {
		return nil
	}
	generations, err := e.repo.GenerationsForWorkspacesTx(ctx, tx, []int{snapshot.WorkspaceID})
	if err != nil {
		return err
	}
	generation, ok := generations[snapshot.WorkspaceID]
	if !ok {
		return nil
	}
	config, err := e.configuration(ctx, snapshot.WorkspaceID, generation)
	if err != nil {
		return err
	}
	if len(config.metrics) == 0 {
		return nil
	}

	ongoingList, err := e.repo.LockOngoingCycles(ctx, tx, []int{itemID})
	if err != nil {
		return err
	}
	ongoing := map[cycleKey]*models.ItemSLACycle{}
	for i := range ongoingList {
		cycle := &ongoingList[i]
		ongoing[cycleKey{cycle.ItemID, cycle.MetricID}] = cycle
	}

	effectiveAt := e.clock.Now()
	for _, metric := range config.metrics {
		target := goalTarget{}
		if len(metric.goals) > 0 {
			resolved, err := e.resolveGoals(ctx, tx, config, metric, []int{itemID}, effectiveAt)
			if err != nil {
				return err
			}
			target = resolved[itemID]
		}
		key := cycleKey{itemID, metric.id}
		if err := e.reconcileMetric(ctx, tx, config, metric, snapshot.WorkspaceID, itemID, snapshot, ongoing[key], target, effectiveAt); err != nil {
			return err
		}
	}
	return nil
}

// reconcileMetric brings one item's cycle for a metric in line with current
// state. It is the same transition table as inline evaluation, driven by
// current-state condition matching.
func (e *Engine) reconcileMetric(ctx context.Context, tx database.Tx, config *compiledConfig, metric *compiledMetric, workspaceID, itemID int, snapshot itemevents.ItemSnapshot, cycle *models.ItemSLACycle, target goalTarget, effectiveAt time.Time) error {
	stop := metric.matchPhaseCurrent(models.SLAPhaseStop, snapshot, config)
	pause := metric.matchPhaseCurrent(models.SLAPhasePause, snapshot, config)
	start := metric.matchPhaseCurrent(models.SLAPhaseStart, snapshot, config)

	if cycle == nil {
		if start && !stop {
			_, err := e.startCycle(ctx, tx, config, metric, workspaceID, itemID, target, pause, effectiveAt, models.SLAOriginBackfill)
			return err
		}
		return nil
	}

	effective := effectiveAt
	if cycle.LastCalculatedAt.After(effective) {
		effective = cycle.LastCalculatedAt
	}
	if stop {
		return e.completeCycle(ctx, tx, config, workspaceID, cycle, effective)
	}
	if err := e.advanceCycle(config, cycle, effective); err != nil {
		return err
	}
	switch {
	case cycle.Paused && !pause:
		if err := e.resumeCycle(ctx, tx, config, metric, workspaceID, cycle, effective); err != nil {
			return err
		}
	case !cycle.Paused && pause:
		if err := e.pauseCycle(ctx, tx, workspaceID, cycle, effective); err != nil {
			return err
		}
	}
	if !cycle.Paused {
		if err := e.reGoalCycle(ctx, tx, config, metric, workspaceID, cycle, target, effective); err != nil {
			return err
		}
	}
	if err := e.repo.UpdateCycle(ctx, tx, cycle); err != nil {
		return err
	}
	// Re-arm jobs so a threshold or goal edit picked up by the recalculation is
	// reflected even when the matched goal did not change.
	if !cycle.Paused {
		return e.syncCycleJobs(ctx, tx, config, metric, cycle, effective)
	}
	return nil
}

// RecalculateMetric reconciles a page of open items for a metric. It returns
// the cursor to resume from and whether the metric is fully reconciled.
func (e *Engine) RecalculateMetric(ctx context.Context, metricID int, cursor string, pageSize int) (next string, done bool, err error) {
	if pageSize <= 0 {
		pageSize = DefaultRecalcPageSize
	}
	err = database.WithTx(e.db, func(tx database.Tx) error {
		var runErr error
		next, done, runErr = e.recalcMetricPageTx(ctx, tx, metricID, cursor, pageSize)
		return runErr
	})
	return next, done, err
}

func (e *Engine) recalcMetricPageTx(ctx context.Context, tx database.Tx, metricID int, cursor string, pageSize int) (nextCursor string, complete bool, err error) {
	metric, err := e.repo.GetMetric(ctx, metricID)
	if err != nil {
		return "", false, err
	}
	afterID := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil {
			return "", false, fmt.Errorf("invalid recalc cursor %q", cursor)
		}
		afterID = parsed
	}

	rows, err := tx.QueryContext(ctx, `SELECT i.id FROM items i
		LEFT JOIN statuses st ON i.status_id = st.id
		LEFT JOIN status_categories sc ON st.category_id = sc.id
		WHERE i.workspace_id = ? AND i.id > ?
		  AND (sc.is_completed IS NULL OR sc.is_completed = false)
		ORDER BY i.id LIMIT ?`, metric.WorkspaceID, afterID, pageSize)
	if err != nil {
		return "", false, fmt.Errorf("page metric items: %w", err)
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return "", false, fmt.Errorf("scan metric item: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return "", false, err
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}

	for _, id := range ids {
		if err := e.recalcItemTx(ctx, tx, id); err != nil {
			return "", false, err
		}
	}
	// Re-arm until a page comes back empty, so a short page still reconciles
	// once more and a crash mid-metric resumes from the last full page.
	if len(ids) == 0 {
		return "", true, nil
	}
	return strconv.Itoa(ids[len(ids)-1]), false, nil
}

func loadItemSnapshotTx(ctx context.Context, tx database.Tx, itemID int) (itemevents.ItemSnapshot, error) {
	var snapshot itemevents.ItemSnapshot
	var statusID, assigneeID, priorityID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id, workspace_id, status_id, assignee_id, priority_id FROM items WHERE id = ?`, itemID).
		Scan(&snapshot.ID, &snapshot.WorkspaceID, &statusID, &assigneeID, &priorityID)
	if err != nil {
		return itemevents.ItemSnapshot{}, err
	}
	if statusID.Valid {
		value := int(statusID.Int64)
		snapshot.StatusID = &value
	}
	if assigneeID.Valid {
		value := int(assigneeID.Int64)
		snapshot.AssigneeID = &value
	}
	if priorityID.Valid {
		value := int(priorityID.Int64)
		snapshot.PriorityID = &value
	}
	return snapshot, nil
}
