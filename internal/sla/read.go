package sla

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"windshift/internal/models"
)

// ItemSLA returns the Jira-shaped SLA state for one item. It never writes;
// elapsed time is extended from stored columns with pure calendar math.
func (e *Engine) ItemSLA(ctx context.Context, itemID, workspaceID int) ([]models.ItemSLA, error) {
	cycles, err := e.repo.ListCyclesForItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	metrics, err := e.repo.ListMetrics(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	metricByID := make(map[int]models.SLAMetric, len(metrics))
	for _, metric := range metrics {
		metricByID[metric.ID] = metric
	}

	recalculating, err := e.isRecalculating(ctx, itemID, workspaceID)
	if err != nil {
		return nil, err
	}

	now := e.clock.Now()
	grouped := map[int][]models.ItemSLACycle{}
	var metricOrder []int
	for _, cycle := range cycles {
		if _, ok := grouped[cycle.MetricID]; !ok {
			metricOrder = append(metricOrder, cycle.MetricID)
		}
		grouped[cycle.MetricID] = append(grouped[cycle.MetricID], cycle)
	}

	states := make([]models.ItemSLA, 0, len(metricOrder))
	for _, metricID := range metricOrder {
		metric := metricByID[metricID]
		state := models.ItemSLA{
			ItemID:        itemID,
			MetricID:      metricID,
			MetricName:    metric.Name,
			DisplayFormat: metric.DisplayFormat,
			Recalculating: recalculating,
		}
		for i := range grouped[metricID] {
			cycle := grouped[metricID][i]
			derived, err := Derive(&cycle, now)
			if err != nil {
				return nil, err
			}
			if cycle.Status == models.SLACycleOngoing {
				state.Ongoing = &derived
			} else {
				state.Completed = append(state.Completed, derived)
			}
		}
		states = append(states, state)
	}
	return states, nil
}

func (e *Engine) isRecalculating(ctx context.Context, itemID, workspaceID int) (bool, error) {
	var recalculating bool
	err := e.db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sla_jobs WHERE kind = 'recalc_item' AND state = 'pending' AND item_id = ?)
		OR EXISTS(
			SELECT 1 FROM sla_jobs j JOIN sla_metrics m ON m.id = j.metric_id
			WHERE j.kind = 'recalc_metric' AND j.state = 'pending' AND m.workspace_id = ?
		)`, itemID, workspaceID).Scan(&recalculating)
	if err != nil {
		return false, err
	}
	return recalculating, nil
}

// Report returns the completed-cycle compliance report for a workspace.
func (e *Engine) Report(ctx context.Context, workspaceID int, from, to *time.Time) (*models.SLAReport, error) {
	return e.repo.SLAReport(ctx, workspaceID, from, to)
}

// WorkspaceForItem resolves the workspace that owns an item.
func (e *Engine) WorkspaceForItem(ctx context.Context, itemID int) (int, error) {
	var workspaceID int
	err := e.db.QueryRowContext(ctx, `SELECT workspace_id FROM items WHERE id = ?`, itemID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("load item workspace: %w", err)
	}
	return workspaceID, nil
}

// ErrNotFound reports that an SLA subject does not exist.
var ErrNotFound = errors.New("sla: not found")
