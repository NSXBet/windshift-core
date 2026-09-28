package sla

import (
	"context"
	"errors"

	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/repository"
)

// SideEffectEmitter emits the notification and automation compatibility events
// for a breach or warning. It is optional; without it the durable state and
// lifecycle history still advance.
type SideEffectEmitter interface {
	EmitBreach(ctx context.Context, tx database.Tx, cycle *models.ItemSLACycle) error
	EmitWarning(ctx context.Context, tx database.Tx, cycle *models.ItemSLACycle, thresholdKey string) error
}

// SetSideEffectEmitter installs the breach/warning side-effect emitter.
func (e *Engine) SetSideEffectEmitter(emitter SideEffectEmitter) { e.sideEffects = emitter }

// RunJob executes one claimed due-work job. A successful run must delete or
// re-arm the job; the loop only reschedules on a returned error.
func (e *Engine) RunJob(ctx context.Context, job models.SLAJob) error {
	switch job.Kind {
	case models.SLAJobBreach:
		return e.runBreachJob(ctx, job)
	case models.SLAJobWarning:
		return e.runWarningJob(ctx, job)
	case models.SLAJobRecalcItem:
		return e.runRecalcItemJob(ctx, job)
	case models.SLAJobRecalcMetric:
		return e.runRecalcMetricJob(ctx, job)
	default:
		return e.repo.FailJob(ctx, job.ID, "unknown job kind "+job.Kind)
	}
}

func (e *Engine) runBreachJob(ctx context.Context, job models.SLAJob) error {
	if job.CycleID == nil || job.DeadlineAt == nil {
		return e.repo.FailJob(ctx, job.ID, "malformed SLA job")
	}
	return database.WithTx(e.db, func(tx database.Tx) error {
		cycle, err := e.repo.GetCycleForUpdate(ctx, tx, *job.CycleID)
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// The cycle moved on: inline evaluation already re-armed whatever is
		// current, so retire this job.
		if cycle.Status != models.SLACycleOngoing || cycle.BreachedAt != nil ||
			cycle.NextDeadlineAt == nil || !cycle.NextDeadlineAt.Equal(*job.DeadlineAt) {
			return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
		}
		deadline := *job.DeadlineAt
		cycle.BreachedAt = &deadline
		cycle.NextDeadlineAt = nil
		cycle.RemainingMs = cycle.GoalDurationMs - cycle.ElapsedMs
		if err := e.repo.UpdateCycle(ctx, tx, cycle); err != nil {
			return err
		}
		// A breach supersedes any pending warning for the cycle.
		if err := e.repo.DeleteJobsForCycleKind(ctx, tx, cycle.ID, models.SLAJobWarning); err != nil {
			return err
		}
		if err := e.appendLifecycle(ctx, tx, e.workspaceForItemTx(ctx, tx, cycle.ItemID), cycle, eventBreached, deadline); err != nil {
			return err
		}
		if e.sideEffects != nil {
			if err := e.sideEffects.EmitBreach(ctx, tx, cycle); err != nil {
				return err
			}
		}
		return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
	})
}

func (e *Engine) runWarningJob(ctx context.Context, job models.SLAJob) error {
	if job.CycleID == nil || job.DeadlineAt == nil {
		return e.repo.FailJob(ctx, job.ID, "malformed SLA job")
	}
	return database.WithTx(e.db, func(tx database.Tx) error {
		cycle, err := e.repo.GetCycleForUpdate(ctx, tx, *job.CycleID)
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if cycle.Status != models.SLACycleOngoing || cycle.BreachedAt != nil ||
			cycle.NextDeadlineAt == nil || !cycle.NextDeadlineAt.Equal(*job.DeadlineAt) {
			return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
		}
		firedAt := e.clock.Now()
		fired, err := e.repo.MarkThresholdFired(ctx, tx, cycle.ID, job.ThresholdKey, firedAt)
		if err != nil {
			return err
		}
		if !fired {
			return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
		}
		if err := e.appendLifecycle(ctx, tx, e.workspaceForItemTx(ctx, tx, cycle.ItemID), cycle, eventWarning, firedAt); err != nil {
			return err
		}
		if e.sideEffects != nil {
			if err := e.sideEffects.EmitWarning(ctx, tx, cycle, job.ThresholdKey); err != nil {
				return err
			}
		}
		return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
	})
}

func (e *Engine) runRecalcItemJob(ctx context.Context, job models.SLAJob) error {
	if job.ItemID == nil {
		return e.repo.FailJob(ctx, job.ID, "malformed SLA job")
	}
	return database.WithTx(e.db, func(tx database.Tx) error {
		if err := e.recalcItemTx(ctx, tx, *job.ItemID); err != nil {
			return err
		}
		return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
	})
}

func (e *Engine) runRecalcMetricJob(ctx context.Context, job models.SLAJob) error {
	if job.MetricID == nil {
		return e.repo.FailJob(ctx, job.ID, "malformed SLA job")
	}
	return database.WithTx(e.db, func(tx database.Tx) error {
		cursor := ""
		if job.Cursor != nil {
			cursor = *job.Cursor
		}
		next, done, err := e.recalcMetricPageTx(ctx, tx, *job.MetricID, cursor, DefaultRecalcPageSize)
		if err != nil {
			return err
		}
		if done {
			return e.repo.DeleteJob(ctx, tx, job.Kind, job.ThresholdKey, job.CycleID, job.ItemID, job.MetricID)
		}
		return e.repo.UpsertJob(ctx, tx, &models.SLAJob{
			Kind:     models.SLAJobRecalcMetric,
			MetricID: job.MetricID,
			DueAt:    e.clock.Now(),
			Cursor:   &next,
		})
	})
}
