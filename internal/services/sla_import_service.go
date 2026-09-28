package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"windshift/internal/businesstime"
	"windshift/internal/database"
	"windshift/internal/jira"
	"windshift/internal/models"
	"windshift/internal/repository"
	"windshift/internal/sla"
)

// ErrSLAImportInvalid marks an import document that cannot be parsed or lacks
// the identities needed to import it safely.
var ErrSLAImportInvalid = errors.New("invalid SLA import document")

// SLAImportSource is stored on imported calendars and metrics so replace mode
// can reconcile exactly the rows this importer owns.
const SLAImportSource = "jira"

// SLAImport modes.
const (
	SLAImportModeMerge   = "merge"
	SLAImportModeReplace = "replace"
)

// SLAImportDocument is the request-scoped Jira SLA export. It is deliberately
// explicit: Jira does not expose SLA configuration through a stable public API,
// so the capture pipeline (or an administrator) supplies this document.
type SLAImportDocument struct {
	Calendars []SLAImportCalendar `json:"calendars"`
	Metrics   []SLAImportMetric   `json:"metrics"`
	Cycles    []SLAImportCycle    `json:"cycles"`
}

type SLAImportCalendar struct {
	SourceID        string                                  `json:"source_id"`
	Name            string                                  `json:"name"`
	Description     string                                  `json:"description"`
	Timezone        string                                  `json:"timezone"`
	WeeklyIntervals map[string][]businesstime.ClockInterval `json:"weekly_intervals"`
	Holidays        []businesstime.Holiday                  `json:"holidays"`
	IsDefault       bool                                    `json:"is_default"`
}

type SLAImportMetric struct {
	SourceID      string               `json:"source_id"`
	Name          string               `json:"name"`
	DisplayFormat string               `json:"display_format"`
	IsActive      *bool                `json:"is_active"`
	Conditions    []SLAImportCondition `json:"conditions"`
	Goals         []SLAImportGoal      `json:"goals"`
}

type SLAImportCondition struct {
	SourceID      string          `json:"source_id"`
	Phase         string          `json:"phase"`
	ConditionType string          `json:"condition_type"`
	Config        json.RawMessage `json:"config"`
}

type SLAImportGoal struct {
	SourceID string            `json:"source_id"`
	Position int               `json:"position"`
	JQL      string            `json:"jql"`
	QL       string            `json:"ql,omitempty"`
	Targets  []SLAImportTarget `json:"targets"`
}

type SLAImportTarget struct {
	SourceID         string `json:"source_id"`
	Position         int    `json:"position"`
	Priority         string `json:"priority"`
	IsFallback       bool   `json:"is_fallback"`
	TargetMs         int64  `json:"target_ms"`
	CalendarSourceID string `json:"calendar_source_id"`
}

type SLAImportCycle struct {
	SourceID         string     `json:"source_id"`
	MetricSourceID   string     `json:"metric_source_id"`
	ItemID           int        `json:"item_id"`
	ItemKey          string     `json:"item_key"`
	GoalSourceID     string     `json:"goal_source_id"`
	CalendarSourceID string     `json:"calendar_source_id"`
	CycleNo          int        `json:"cycle_no"`
	Status           string     `json:"status"`
	StartedAt        time.Time  `json:"started_at"`
	StoppedAt        *time.Time `json:"stopped_at"`
	ElapsedMs        int64      `json:"elapsed_ms"`
	GoalDurationMs   int64      `json:"goal_duration_ms"`
	BreachedAt       *time.Time `json:"breached_at"`
	BreachTime       *time.Time `json:"breach_time"`
	Paused           bool       `json:"paused"`
	PauseStartedAt   *time.Time `json:"pause_started_at"`
}

// SLAImportCounts summarizes planned or applied writes.
type SLAImportCounts struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Skip   int `json:"skip"`
}

// SLAImportIssue is one needs-attention or warning entry.
type SLAImportIssue struct {
	Entity   string `json:"entity"`
	SourceID string `json:"source_id,omitempty"`
	Message  string `json:"message"`
}

// SLAImportResult is the preview and import response.
type SLAImportResult struct {
	Mode           string           `json:"mode"`
	Calendars      SLAImportCounts  `json:"calendars"`
	Metrics        SLAImportCounts  `json:"metrics"`
	Cycles         SLAImportCounts  `json:"cycles"`
	NeedsAttention []SLAImportIssue `json:"needs_attention"`
	Warnings       []SLAImportIssue `json:"warnings"`
}

// SLAImportService imports Jira SLA configuration and cycle facts. It is
// request-scoped: preview resolves everything without writing, import applies
// the same resolution in one transaction per configuration step.
type SLAImportService struct {
	db     database.Database
	repo   *repository.SLARepository
	engine *sla.Engine
}

// NewSLAImportService constructs the import service.
func NewSLAImportService(db database.Database, engine *sla.Engine) *SLAImportService {
	return &SLAImportService{db: db, repo: repository.NewSLARepository(db), engine: engine}
}

// resolvedMetric carries the persisted shape plus import bookkeeping.
type resolvedMetric struct {
	metric      *models.SLAMetric
	exercisable bool // every goal translated and every target resolved
}

// Preview resolves a document without writing and reports what would change.
func (s *SLAImportService) Preview(ctx context.Context, workspaceID int, document SLAImportDocument) (*SLAImportResult, error) {
	result, _, _, _, err := s.resolve(ctx, workspaceID, document)
	if err != nil {
		return nil, err
	}
	result.Mode = SLAImportModeMerge
	return result, nil
}

// Import applies a document in merge or replace mode.
func (s *SLAImportService) Import(ctx context.Context, workspaceID int, document SLAImportDocument, mode string) (*SLAImportResult, error) {
	if mode != SLAImportModeMerge && mode != SLAImportModeReplace {
		return nil, fmt.Errorf("%w: mode must be merge or replace", ErrSLAImportInvalid)
	}
	result, calendars, metrics, cycles, err := s.resolve(ctx, workspaceID, document)
	if err != nil {
		return nil, err
	}
	result.Mode = mode

	// Persist calendars first: metrics and cycles reference them.
	calendarIDs := map[string]int{}
	err = database.WithTx(s.db, func(tx database.Tx) error {
		for _, calendar := range calendars {
			id, err := s.persistCalendar(ctx, tx, calendar)
			if err != nil {
				return err
			}
			calendarIDs[valueOrEmpty(calendar.SourceID)] = id
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	metricIDs := map[string]int{}
	goalIDsByMetric := map[string]map[string]int{}
	err = database.WithTx(s.db, func(tx database.Tx) error {
		for _, resolved := range metrics {
			id, goalIDs, err := s.persistMetric(ctx, tx, resolved, calendarIDs)
			if err != nil {
				return err
			}
			metricIDs[valueOrEmpty(resolved.metric.SourceID)] = id
			goalIDsByMetric[valueOrEmpty(resolved.metric.SourceID)] = goalIDs
			if _, err := s.engine.BumpConfigGeneration(ctx, tx, workspaceID); err != nil {
				return err
			}
			metric := id
			if err := s.repo.UpsertJob(ctx, tx, &models.SLAJob{Kind: models.SLAJobRecalcMetric, MetricID: &metric, DueAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	snapshots := map[string]json.RawMessage{}
	for _, calendar := range calendars {
		snapshot, err := importCalendarSnapshot(calendar)
		if err != nil {
			return nil, err
		}
		snapshots[valueOrEmpty(calendar.SourceID)] = snapshot
	}

	// Resolve cycle inputs before the write transaction: SQLite serializes
	// writers, so repository reads must not run while the transaction holds the
	// write lock.
	type cycleWrite struct {
		cycle      *SLAImportCycle
		metricID   int
		itemID     int
		calendarID int
		goalID     int
		snapshot   json.RawMessage
		existingID int64
	}
	writes := make([]cycleWrite, 0, len(cycles))
	for i := range cycles {
		cycle := &cycles[i]
		metricID := metricIDs[cycle.MetricSourceID]
		if metricID == 0 {
			result.Warnings = append(result.Warnings, SLAImportIssue{Entity: "cycle", SourceID: cycle.SourceID, Message: "metric was not imported"})
			result.Cycles.Skip++
			continue
		}
		itemID, err := s.resolveItemID(ctx, workspaceID, cycle.ItemID, cycle.ItemKey)
		if err != nil {
			result.NeedsAttention = append(result.NeedsAttention, SLAImportIssue{Entity: "cycle", SourceID: cycle.SourceID, Message: err.Error()})
			result.Cycles.Skip++
			continue
		}
		existingID := int64(0)
		if existing, err := s.repo.GetCycleBySourceID(ctx, metricID, cycle.SourceID); err == nil {
			existingID = existing.ID
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
		writes = append(writes, cycleWrite{
			cycle:      cycle,
			metricID:   metricID,
			itemID:     itemID,
			calendarID: calendarIDs[cycle.CalendarSourceID],
			goalID:     goalIDsByMetric[cycle.MetricSourceID][cycle.GoalSourceID],
			snapshot:   snapshots[cycle.CalendarSourceID],
			existingID: existingID,
		})
	}
	err = database.WithTx(s.db, func(tx database.Tx) error {
		for _, write := range writes {
			if err := s.writeCycle(ctx, tx, write.metricID, write.itemID, write.calendarID, write.goalID, write.snapshot, write.existingID, write.cycle); err != nil {
				return err
			}
			if write.existingID == 0 {
				result.Cycles.Create++
			} else {
				result.Cycles.Update++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if mode == SLAImportModeReplace {
		if err := s.reconcileReplace(ctx, workspaceID, document, metricIDs, result); err != nil {
			return nil, err
		}
	}
	if s.engine != nil {
		s.engine.Wake()
	}
	return result, nil
}

// resolve builds the persisted shapes and the preview counts without writing.
func (s *SLAImportService) resolve(ctx context.Context, workspaceID int, document SLAImportDocument) (*SLAImportResult, []*models.WorkingCalendar, []resolvedMetric, []SLAImportCycle, error) {
	result := &SLAImportResult{Warnings: []SLAImportIssue{}, NeedsAttention: []SLAImportIssue{}}

	calendars := make([]*models.WorkingCalendar, 0, len(document.Calendars))
	calendarBySource := map[string]*models.WorkingCalendar{}
	for i := range document.Calendars {
		source := document.Calendars[i]
		if strings.TrimSpace(source.SourceID) == "" {
			return nil, nil, nil, nil, fmt.Errorf("%w: calendar %d has no source_id", ErrSLAImportInvalid, i)
		}
		calendar := buildImportCalendar(workspaceID, source)
		if existing, err := s.repo.GetCalendarBySourceID(ctx, workspaceID, source.SourceID); err == nil {
			calendar.ID = existing.ID
			result.Calendars.Update++
		} else if errors.Is(err, repository.ErrNotFound) {
			result.Calendars.Create++
		} else {
			return nil, nil, nil, nil, err
		}
		calendars = append(calendars, calendar)
		calendarBySource[source.SourceID] = calendar
	}

	metrics := make([]resolvedMetric, 0, len(document.Metrics))
	for i := range document.Metrics {
		metric, issues := s.resolveMetric(ctx, workspaceID, document.Metrics[i], calendarBySource)
		result.NeedsAttention = append(result.NeedsAttention, issues...)
		if existing, err := s.repo.GetMetricBySourceID(ctx, workspaceID, document.Metrics[i].SourceID); err == nil {
			metric.metric.ID = existing.ID
			result.Metrics.Update++
		} else if errors.Is(err, repository.ErrNotFound) {
			result.Metrics.Create++
		} else {
			return nil, nil, nil, nil, err
		}
		if !metric.exercisable {
			active := false
			metric.metric.IsActive = active
		}
		metrics = append(metrics, metric)
	}

	// Cycles are validated during import once metric/calendar ids are known;
	// the preview reports only structural issues here.
	for i := range document.Cycles {
		cycle := document.Cycles[i]
		if strings.TrimSpace(cycle.SourceID) == "" {
			result.NeedsAttention = append(result.NeedsAttention, SLAImportIssue{Entity: "cycle", Message: "cycle has no source_id"})
			result.Cycles.Skip++
			continue
		}
		if strings.TrimSpace(cycle.MetricSourceID) == "" {
			result.NeedsAttention = append(result.NeedsAttention, SLAImportIssue{Entity: "cycle", SourceID: cycle.SourceID, Message: "cycle has no metric_source_id"})
			result.Cycles.Skip++
			continue
		}
		result.Cycles.Create++
	}

	return result, calendars, metrics, document.Cycles, nil
}

func (s *SLAImportService) resolveMetric(ctx context.Context, workspaceID int, source SLAImportMetric, calendars map[string]*models.WorkingCalendar) (resolvedMetric, []SLAImportIssue) {
	metric := &models.SLAMetric{
		WorkspaceID:   workspaceID,
		Name:          strings.TrimSpace(source.Name),
		DisplayFormat: source.DisplayFormat,
		IsActive:      source.IsActive == nil || *source.IsActive,
		ImportStatus:  "ready",
		Source:        stringPtr(SLAImportSource),
		SourceID:      stringPtr(source.SourceID),
	}
	if metric.DisplayFormat == "" {
		metric.DisplayFormat = "time"
	}
	if metric.DisplayFormat != "time" && metric.DisplayFormat != "due_date" {
		metric.DisplayFormat = "time"
	}
	issues := []SLAImportIssue{}
	exercisable := true

	conditions := make([]models.SLACondition, 0, len(source.Conditions))
	for i, condition := range source.Conditions {
		config := condition.Config
		if len(config) == 0 {
			config = json.RawMessage("{}")
		}
		conditions = append(conditions, models.SLACondition{
			Phase:         condition.Phase,
			Position:      i,
			ConditionType: condition.ConditionType,
			Config:        config,
			SourcePayload: stringPtr(string(config)),
		})
	}
	metric.Conditions = conditions

	for i, goal := range source.Goals {
		ql := strings.TrimSpace(goal.QL)
		status := "ready"
		if ql == "" {
			translated, unsupported := jira.TranslateJQLToWindshiftQL(goal.JQL, workspaceID)
			if len(unsupported) > 0 {
				status = "needs_attention"
				exercisable = false
				for _, clause := range unsupported {
					issues = append(issues, SLAImportIssue{Entity: "goal", SourceID: goal.SourceID, Message: "untranslated JQL clause: " + clause})
				}
				ql = translated
			} else {
				ql = translated
			}
		}
		if strings.TrimSpace(ql) == "" {
			ql = "1 = 0"
		}
		resolvedGoal := models.SLAGoal{
			Position:     i,
			QLQuery:      ql,
			ImportStatus: status,
			SourceID:     stringPtr(goal.SourceID),
		}
		if status == "needs_attention" {
			resolvedGoal.OriginalJQL = stringPtr(goal.JQL)
		}
		for j, target := range goal.Targets {
			resolvedTarget := models.SLAGoalTarget{
				Position:   j,
				IsFallback: target.IsFallback,
				TargetMs:   target.TargetMs,
				SourceID:   stringPtr(target.SourceID),
			}
			if !target.IsFallback && strings.TrimSpace(target.Priority) != "" {
				if priorityID, err := s.repo.PriorityIDByName(ctx, target.Priority); err == nil {
					resolvedTarget.PriorityID = &priorityID
				} else {
					exercisable = false
					resolvedGoal.ImportStatus = "needs_attention"
					issues = append(issues, SLAImportIssue{Entity: "target", SourceID: target.SourceID, Message: "priority not found: " + target.Priority})
				}
			}
			if calendar, ok := calendars[target.CalendarSourceID]; ok {
				resolvedTarget.CalendarID = calendar.ID
				if calendar.ID == 0 {
					// New calendar; resolve after persistence. Keep the source
					// id so import can remap it.
					resolvedTarget.SourcePayload = stringPtr("calendar_source_id:" + target.CalendarSourceID)
				}
			} else {
				exercisable = false
				resolvedGoal.ImportStatus = "needs_attention"
				issues = append(issues, SLAImportIssue{Entity: "target", SourceID: target.SourceID, Message: "calendar not found: " + target.CalendarSourceID})
			}
			resolvedGoal.Targets = append(resolvedGoal.Targets, resolvedTarget)
		}
		metric.Goals = append(metric.Goals, resolvedGoal)
	}
	if !exercisable {
		metric.ImportStatus = "needs_attention"
	}
	return resolvedMetric{metric: metric, exercisable: exercisable}, issues
}

func (s *SLAImportService) persistCalendar(ctx context.Context, tx database.Tx, calendar *models.WorkingCalendar) (int, error) {
	if calendar.ID != 0 {
		if err := s.repo.UpdateCalendarTx(ctx, tx, calendar); err != nil {
			return 0, err
		}
		return calendar.ID, nil
	}
	id, err := s.repo.CreateCalendarTx(ctx, tx, calendar)
	if err != nil {
		return 0, err
	}
	calendar.ID = id
	return id, nil
}

func (s *SLAImportService) persistMetric(ctx context.Context, tx database.Tx, resolved resolvedMetric, calendarIDs map[string]int) (metricID int, goalIDs map[string]int, err error) {
	metric := resolved.metric
	// Remap target calendars newly created in this import.
	for i := range metric.Goals {
		for j := range metric.Goals[i].Targets {
			target := &metric.Goals[i].Targets[j]
			if target.CalendarID == 0 && target.SourcePayload != nil {
				sourceID := strings.TrimPrefix(*target.SourcePayload, "calendar_source_id:")
				target.CalendarID = calendarIDs[sourceID]
				target.SourcePayload = nil
			}
		}
	}
	if metric.ID != 0 {
		err = s.repo.UpdateMetric(ctx, tx, metric)
		metricID = metric.ID
	} else {
		metricID, err = s.repo.CreateMetric(ctx, tx, metric)
	}
	if err != nil {
		return 0, nil, err
	}
	goalIDs, err = s.repo.GoalIDsBySourceID(ctx, tx, metricID)
	if err != nil {
		return 0, nil, err
	}
	return metricID, goalIDs, nil
}

func (s *SLAImportService) writeCycle(ctx context.Context, tx database.Tx, metricID, itemID, calendarID, goalID int, snapshot json.RawMessage, existingID int64, source *SLAImportCycle) error {
	status := source.Status
	if status == "" {
		status = models.SLACycleOngoing
	}
	cycleNo := source.CycleNo
	if cycleNo <= 0 {
		cycleNo = 1
	}
	now := time.Now().UTC()
	cycle := &models.ItemSLACycle{
		ItemID:           itemID,
		MetricID:         metricID,
		CycleNo:          cycleNo,
		Status:           status,
		StartedAt:        source.StartedAt,
		StoppedAt:        source.StoppedAt,
		BreachTime:       source.BreachTime,
		BreachedAt:       source.BreachedAt,
		GoalDurationMs:   source.GoalDurationMs,
		ElapsedMs:        source.ElapsedMs,
		RemainingMs:      source.GoalDurationMs - source.ElapsedMs,
		Paused:           source.Paused,
		PauseStartedAt:   source.PauseStartedAt,
		Breached:         source.BreachedAt != nil,
		LastCalculatedAt: now,
		Origin:           models.SLAOriginImport,
		SourceID:         stringPtr(source.SourceID),
	}
	if goalID != 0 {
		cycle.GoalID = &goalID
	}
	if calendarID != 0 {
		cycle.CalendarID = &calendarID
	}
	if len(snapshot) > 0 {
		cycle.CalendarSnapshot = snapshot
	} else {
		cycle.CalendarSnapshot = json.RawMessage(`{"timezone":"UTC","weekly_intervals":{}}`)
	}
	if status == models.SLACycleOngoing {
		if calendar, err := businesstime.CompileJSON(string(cycle.CalendarSnapshot)); err == nil {
			remaining := time.Duration(cycle.GoalDurationMs-cycle.ElapsedMs) * time.Millisecond
			if remaining > 0 {
				if deadline, ok := calendar.AddCalendarTime(now, remaining); ok {
					cycle.NextDeadlineAt = &deadline
				}
			}
		}
	}

	if existingID == 0 {
		cycleID, err := s.repo.InsertCycle(ctx, tx, cycle)
		if err != nil {
			return err
		}
		cycle.ID = cycleID
	} else {
		cycle.ID = existingID
		if err := s.repo.UpdateCycle(ctx, tx, cycle); err != nil {
			return err
		}
	}
	if cycle.Status == models.SLACycleOngoing && cycle.NextDeadlineAt != nil {
		deadline := *cycle.NextDeadlineAt
		cycleRef := cycle.ID
		job := &models.SLAJob{Kind: models.SLAJobBreach, CycleID: &cycleRef, DueAt: deadline, DeadlineAt: &deadline}
		if err := s.repo.UpsertJob(ctx, tx, job); err != nil {
			return err
		}
	}
	return nil
}

func (s *SLAImportService) reconcileReplace(ctx context.Context, workspaceID int, document SLAImportDocument, metricIDs map[string]int, result *SLAImportResult) error {
	keepMetrics := map[string]bool{}
	for _, metric := range document.Metrics {
		keepMetrics[metric.SourceID] = true
	}
	existingMetrics, err := s.repo.ListImportedMetricSourceIDs(ctx, workspaceID, SLAImportSource)
	if err != nil {
		return err
	}
	for _, sourceID := range existingMetrics {
		if keepMetrics[sourceID] {
			continue
		}
		stale, err := s.repo.GetMetricBySourceID(ctx, workspaceID, sourceID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return err
		}
		if err := database.WithTx(s.db, func(tx database.Tx) error {
			if err := s.repo.DeleteMetric(ctx, tx, stale.ID); err != nil {
				return err
			}
			_, err := s.engine.BumpConfigGeneration(ctx, tx, workspaceID)
			return err
		}); err != nil {
			return err
		}
		result.Warnings = append(result.Warnings, SLAImportIssue{Entity: "metric", SourceID: sourceID, Message: "removed by replace import"})
	}

	keepCalendars := map[string]bool{}
	for _, calendar := range document.Calendars {
		keepCalendars[calendar.SourceID] = true
	}
	existingCalendars, err := s.repo.ListImportedCalendarSourceIDs(ctx, workspaceID, SLAImportSource)
	if err != nil {
		return err
	}
	for _, sourceID := range existingCalendars {
		if keepCalendars[sourceID] {
			continue
		}
		stale, err := s.repo.GetCalendarBySourceID(ctx, workspaceID, sourceID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return err
		}
		if err := s.repo.DeleteCalendar(ctx, stale.ID); err != nil {
			result.NeedsAttention = append(result.NeedsAttention, SLAImportIssue{Entity: "calendar", SourceID: sourceID, Message: "cannot remove: " + err.Error()})
			continue
		}
		result.Warnings = append(result.Warnings, SLAImportIssue{Entity: "calendar", SourceID: sourceID, Message: "removed by replace import"})
	}
	_ = metricIDs
	return nil
}

func (s *SLAImportService) resolveItemID(ctx context.Context, workspaceID, itemID int, key string) (int, error) {
	if itemID > 0 {
		return itemID, nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, fmt.Errorf("cycle has neither item_id nor item_key")
	}
	workspaceKey, err := s.repo.WorkspaceKey(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	prefix, number, ok := strings.Cut(key, "-")
	if !ok {
		return 0, fmt.Errorf("item_key %q must be %s-<number>", key, workspaceKey)
	}
	if !strings.EqualFold(prefix, workspaceKey) {
		return 0, fmt.Errorf("item_key %q does not belong to workspace %s", key, workspaceKey)
	}
	parsed, err := strconv.Atoi(number)
	if err != nil {
		return 0, fmt.Errorf("item_key %q has a non-numeric number", key)
	}
	id, err := s.repo.ItemIDByWorkspaceNumber(ctx, workspaceID, parsed)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return 0, fmt.Errorf("item %q was not imported", key)
		}
		return 0, err
	}
	return id, nil
}

func buildImportCalendar(workspaceID int, source SLAImportCalendar) *models.WorkingCalendar {
	weekly, _ := json.Marshal(source.WeeklyIntervals)
	holidays, _ := json.Marshal(source.Holidays)
	if len(weekly) == 0 || string(weekly) == "null" {
		weekly = []byte("{}")
	}
	if len(holidays) == 0 || string(holidays) == "null" {
		holidays = []byte("[]")
	}
	timezone := strings.TrimSpace(source.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	return &models.WorkingCalendar{
		WorkspaceID:     &workspaceID,
		Name:            strings.TrimSpace(source.Name),
		Description:     source.Description,
		Timezone:        timezone,
		WeeklyIntervals: weekly,
		Holidays:        holidays,
		IsDefault:       source.IsDefault,
		Source:          stringPtr(SLAImportSource),
		SourceID:        stringPtr(source.SourceID),
	}
}

func importCalendarSnapshot(calendar *models.WorkingCalendar) (json.RawMessage, error) {
	raw, err := businesstime.FromStored(calendar.Timezone, calendar.WeeklyIntervals, calendar.Holidays)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
