package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"windshift/internal/database"
	"windshift/internal/models"
	"windshift/internal/repository"
	"windshift/internal/restapi"
	"windshift/internal/services"
	"windshift/internal/sla"
)

// SLAHandler serves SLA configuration and item SLA state.
type SLAHandler struct {
	db                database.Database
	repo              *repository.SLARepository
	teamRepo          *repository.TeamRepository
	engine            *sla.Engine
	calendars         *services.SLACalendarService
	settings          *services.SLASettingsService
	bindings          *services.SLATeamBindingService
	permissionService *services.PermissionService
}

// NewSLAHandler constructs an SLAHandler.
func NewSLAHandler(db database.Database, engine *sla.Engine, permissionService *services.PermissionService) *SLAHandler {
	return &SLAHandler{
		db:                db,
		repo:              repository.NewSLARepository(db),
		teamRepo:          repository.NewTeamRepository(db),
		engine:            engine,
		calendars:         services.NewSLACalendarService(db, engine),
		settings:          services.NewSLASettingsService(db, engine),
		bindings:          services.NewSLATeamBindingService(db, permissionService),
		permissionService: permissionService,
	}
}

// ---------------------------------------------------------------------------
// Item SLA state
// ---------------------------------------------------------------------------

// GetItemSLA returns the Jira-shaped SLA state for an item. It never writes.
func (h *SLAHandler) GetItemSLA(w http.ResponseWriter, r *http.Request) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	itemID, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()
	var workspaceID int
	if err := h.db.QueryRowContext(ctx, `SELECT workspace_id FROM items WHERE id = ?`, itemID).Scan(&workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondNotFound(w, r, "item")
			return
		}
		respondError(w, r, slaInternal(err))
		return
	}
	if !RequireWorkspacePermission(w, r, user.ID, workspaceID, models.PermissionItemView, h.permissionService) {
		return
	}
	states, err := h.engine.ItemSLA(ctx, itemID, workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, states)
}

// ---------------------------------------------------------------------------
// Workspace calendars
// ---------------------------------------------------------------------------

type slaCalendarRequest struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Timezone        string          `json:"timezone"`
	WeeklyIntervals json.RawMessage `json:"weekly_intervals"`
	Holidays        json.RawMessage `json:"holidays"`
	IsDefault       bool            `json:"is_default"`
	ApplyToOngoing  bool            `json:"apply_to_ongoing"`
}

// ListWorkspaceCalendars returns a workspace's owned calendars.
func (h *SLAHandler) ListWorkspaceCalendars(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	calendars, err := h.calendars.ListWorkspace(r.Context(), workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, calendars)
}

// ListAvailableCalendars returns workspace calendars plus team calendars
// authorized through an active binding.
func (h *SLAHandler) ListAvailableCalendars(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	calendars, err := h.calendars.ListAvailable(r.Context(), workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, calendars)
}

// CreateWorkspaceCalendar creates a workspace-owned calendar.
func (h *SLAHandler) CreateWorkspaceCalendar(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	request, ok := decodeJSON[slaCalendarRequest](w, r)
	if !ok {
		return
	}
	calendar, err := h.calendars.CreateWorkspace(r.Context(), workspaceID, request.input())
	if !h.writeCalendarResult(w, r, err) {
		return
	}
	respondJSONCreated(w, calendar)
}

// UpdateWorkspaceCalendar updates a workspace-owned calendar.
func (h *SLAHandler) UpdateWorkspaceCalendar(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	calendarID, ok := requireIDParam(w, r, "calendarId")
	if !ok {
		return
	}
	request, ok := decodeJSON[slaCalendarRequest](w, r)
	if !ok {
		return
	}
	calendar, err := h.calendars.UpdateWorkspace(r.Context(), workspaceID, calendarID, request.input())
	if !h.writeCalendarResult(w, r, err) {
		return
	}
	respondJSONOK(w, calendar)
}

// DeleteWorkspaceCalendar deletes a workspace-owned calendar.
func (h *SLAHandler) DeleteWorkspaceCalendar(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	calendarID, ok := requireIDParam(w, r, "calendarId")
	if !ok {
		return
	}
	if !h.writeCalendarDelete(w, r, h.calendars.DeleteWorkspace(r.Context(), workspaceID, calendarID)) {
		return
	}
	respondJSONOK(w, map[string]bool{"deleted": true})
}

// ---------------------------------------------------------------------------
// Team calendars
// ---------------------------------------------------------------------------

// ListTeamCalendars returns a team's shared calendars.
func (h *SLAHandler) ListTeamCalendars(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	calendars, err := h.calendars.ListTeam(r.Context(), teamID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, calendars)
}

// CreateTeamCalendar creates a team-shared calendar.
func (h *SLAHandler) CreateTeamCalendar(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	request, ok := decodeJSON[slaCalendarRequest](w, r)
	if !ok {
		return
	}
	calendar, err := h.calendars.CreateTeam(r.Context(), teamID, request.input())
	if !h.writeCalendarResult(w, r, err) {
		return
	}
	respondJSONCreated(w, calendar)
}

// UpdateTeamCalendar updates a team-shared calendar.
func (h *SLAHandler) UpdateTeamCalendar(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	calendarID, ok := requireIDParam(w, r, "calendarId")
	if !ok {
		return
	}
	request, ok := decodeJSON[slaCalendarRequest](w, r)
	if !ok {
		return
	}
	calendar, err := h.calendars.UpdateTeam(r.Context(), teamID, calendarID, request.input())
	if !h.writeCalendarResult(w, r, err) {
		return
	}
	respondJSONOK(w, calendar)
}

// DeleteTeamCalendar deletes a team-shared calendar.
func (h *SLAHandler) DeleteTeamCalendar(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	calendarID, ok := requireIDParam(w, r, "calendarId")
	if !ok {
		return
	}
	if !h.writeCalendarDelete(w, r, h.calendars.DeleteTeam(r.Context(), teamID, calendarID)) {
		return
	}
	respondJSONOK(w, map[string]bool{"deleted": true})
}

// input maps the transport request onto the shared calendar service input.
func (request slaCalendarRequest) input() services.SLACalendarInput {
	return services.SLACalendarInput{
		Name:            request.Name,
		Description:     request.Description,
		Timezone:        request.Timezone,
		WeeklyIntervals: request.WeeklyIntervals,
		Holidays:        request.Holidays,
		IsDefault:       request.IsDefault,
		ApplyToOngoing:  request.ApplyToOngoing,
	}
}

// writeCalendarResult maps service errors to responses. It returns true when
// the caller should write the success body.
func (h *SLAHandler) writeCalendarResult(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, services.ErrSLACalendarInvalid):
		respondValidationError(w, r, err.Error())
	case errors.Is(err, repository.ErrNotFound):
		respondNotFound(w, r, "calendar")
	case errors.Is(err, repository.ErrSLAInUse):
		respondError(w, r, restapi.NewAPIError(http.StatusConflict, restapi.ErrCodeConflict, err.Error()))
	default:
		respondError(w, r, slaInternal(err))
	}
	return false
}

func (h *SLAHandler) writeCalendarDelete(w http.ResponseWriter, r *http.Request, err error) bool {
	return h.writeCalendarResult(w, r, err)
}

// ---------------------------------------------------------------------------
// Team-workspace bindings
// ---------------------------------------------------------------------------

type slaBindingRequest struct {
	TeamID      int `json:"team_id"`
	WorkspaceID int `json:"workspace_id"`
}

// ListWorkspaceTeamBindings returns the teams bound to a workspace.
func (h *SLAHandler) ListWorkspaceTeamBindings(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	bindings, err := h.bindings.ListForWorkspace(r.Context(), workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, bindings)
}

// CreateWorkspaceTeamBinding binds a team to a workspace after both sides
// consent.
func (h *SLAHandler) CreateWorkspaceTeamBinding(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	request, ok := decodeJSON[slaBindingRequest](w, r)
	if !ok {
		return
	}
	binding, err := h.bindings.Create(r.Context(), user.ID, request.TeamID, workspaceID)
	if !h.writeBindingResult(w, r, err) {
		return
	}
	respondJSONCreated(w, binding)
}

// DeleteWorkspaceTeamBinding removes a workspace-scoped binding after both
// sides consent.
func (h *SLAHandler) DeleteWorkspaceTeamBinding(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	bindingID, ok := requireIDParam(w, r, "bindingId")
	if !ok {
		return
	}
	if !h.writeBindingResult(w, r, h.bindings.DeleteForWorkspace(r.Context(), user.ID, workspaceID, bindingID)) {
		return
	}
	respondJSONOK(w, map[string]bool{"deleted": true})
}

// ListTeamWorkspaceBindings returns the workspaces bound to a team.
func (h *SLAHandler) ListTeamWorkspaceBindings(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	bindings, err := h.bindings.ListForTeam(r.Context(), teamID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, bindings)
}

// CreateTeamWorkspaceBinding binds a workspace to a team after both sides
// consent.
func (h *SLAHandler) CreateTeamWorkspaceBinding(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	request, ok := decodeJSON[slaBindingRequest](w, r)
	if !ok {
		return
	}
	binding, err := h.bindings.Create(r.Context(), user.ID, teamID, request.WorkspaceID)
	if !h.writeBindingResult(w, r, err) {
		return
	}
	respondJSONCreated(w, binding)
}

// DeleteTeamWorkspaceBinding removes a team-scoped binding after both sides
// consent.
func (h *SLAHandler) DeleteTeamWorkspaceBinding(w http.ResponseWriter, r *http.Request) {
	teamID, ok := h.authorizeTeamAdmin(w, r)
	if !ok {
		return
	}
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	bindingID, ok := requireIDParam(w, r, "bindingId")
	if !ok {
		return
	}
	if !h.writeBindingResult(w, r, h.bindings.DeleteForTeam(r.Context(), user.ID, teamID, bindingID)) {
		return
	}
	respondJSONOK(w, map[string]bool{"deleted": true})
}

// writeBindingResult maps binding errors to responses. It returns true when
// the caller should write the success body.
func (h *SLAHandler) writeBindingResult(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, services.ErrSLABindingForbidden):
		respondError(w, r, restapi.NewAPIError(http.StatusForbidden, restapi.ErrCodeForbidden, err.Error()))
	case errors.Is(err, repository.ErrDuplicateEntry):
		respondError(w, r, restapi.NewAPIError(http.StatusConflict, restapi.ErrCodeConflict, "team is already bound to this workspace"))
	case errors.Is(err, repository.ErrNotFound):
		respondNotFound(w, r, "binding")
	case errors.Is(err, repository.ErrSLAInUse):
		respondError(w, r, restapi.NewAPIError(http.StatusConflict, restapi.ErrCodeConflict, err.Error()))
	default:
		respondError(w, r, slaInternal(err))
	}
	return false
}

// ---------------------------------------------------------------------------
// Warning thresholds
// ---------------------------------------------------------------------------

type slaWarningThresholdRequest struct {
	Label    string `json:"label"`
	Percent  int    `json:"percent"`
	MetricID *int   `json:"metric_id"`
	IsActive *bool  `json:"is_active"`
}

func (request slaWarningThresholdRequest) input() services.SLAWarningThresholdInput {
	isActive := true
	if request.IsActive != nil {
		isActive = *request.IsActive
	}
	return services.SLAWarningThresholdInput{
		Label:    request.Label,
		Percent:  request.Percent,
		MetricID: request.MetricID,
		IsActive: isActive,
	}
}

// ListWarningThresholds returns a workspace's SLA warning thresholds.
func (h *SLAHandler) ListWarningThresholds(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	thresholds, err := h.settings.ListWarningThresholds(r.Context(), workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, thresholds)
}

// CreateWarningThreshold creates a warning threshold.
func (h *SLAHandler) CreateWarningThreshold(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	request, ok := decodeJSON[slaWarningThresholdRequest](w, r)
	if !ok {
		return
	}
	threshold, err := h.settings.CreateWarningThreshold(r.Context(), workspaceID, request.input())
	if !h.writeThresholdResult(w, r, err) {
		return
	}
	respondJSONCreated(w, threshold)
}

// UpdateWarningThreshold updates a warning threshold.
func (h *SLAHandler) UpdateWarningThreshold(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	thresholdID, ok := requireIDParam(w, r, "thresholdId")
	if !ok {
		return
	}
	request, ok := decodeJSON[slaWarningThresholdRequest](w, r)
	if !ok {
		return
	}
	threshold, err := h.settings.UpdateWarningThreshold(r.Context(), workspaceID, thresholdID, request.input())
	if !h.writeThresholdResult(w, r, err) {
		return
	}
	respondJSONOK(w, threshold)
}

// DeleteWarningThreshold deletes a warning threshold.
func (h *SLAHandler) DeleteWarningThreshold(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	thresholdID, ok := requireIDParam(w, r, "thresholdId")
	if !ok {
		return
	}
	if !h.writeThresholdResult(w, r, h.settings.DeleteWarningThreshold(r.Context(), workspaceID, thresholdID)) {
		return
	}
	respondJSONOK(w, map[string]bool{"deleted": true})
}

func (h *SLAHandler) writeThresholdResult(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, services.ErrSLAThresholdInvalid):
		respondValidationError(w, r, err.Error())
	case errors.Is(err, repository.ErrNotFound):
		respondNotFound(w, r, "warning threshold")
	default:
		respondError(w, r, slaInternal(err))
	}
	return false
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

type slaMetricRequest struct {
	Name          string                `json:"name"`
	DisplayFormat string                `json:"display_format"`
	Position      int                   `json:"position"`
	IsActive      *bool                 `json:"is_active"`
	ImportStatus  string                `json:"import_status"`
	Conditions    []models.SLACondition `json:"conditions"`
	Goals         []models.SLAGoal      `json:"goals"`
}

// ListMetrics returns a workspace's SLA metrics.
func (h *SLAHandler) ListMetrics(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	metrics, err := h.repo.ListMetrics(r.Context(), workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, metrics)
}

// GetMetric returns one SLA metric.
func (h *SLAHandler) GetMetric(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	metricID, ok := requireIDParam(w, r, "metricId")
	if !ok {
		return
	}
	metric, err := h.repo.GetMetric(r.Context(), metricID)
	if err != nil || metric.WorkspaceID != workspaceID {
		respondNotFound(w, r, "metric")
		return
	}
	respondJSONOK(w, metric)
}

// CreateMetric creates a metric and schedules recalculation.
func (h *SLAHandler) CreateMetric(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	request, ok := decodeJSON[slaMetricRequest](w, r)
	if !ok {
		return
	}
	metric, ok := h.buildMetric(w, r, workspaceID, request)
	if !ok {
		return
	}
	ctx := r.Context()
	var metricID int
	err := database.WithTx(h.db, func(tx database.Tx) error {
		id, err := h.repo.CreateMetric(ctx, tx, metric)
		if err != nil {
			return err
		}
		metricID = id
		if _, err := h.engine.BumpConfigGeneration(ctx, tx, workspaceID); err != nil {
			return err
		}
		return h.enqueueRecalcMetric(ctx, tx, metricID)
	})
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	metric.ID = metricID
	h.engine.Wake()
	respondJSONCreated(w, metric)
}

// UpdateMetric updates a metric and schedules recalculation.
func (h *SLAHandler) UpdateMetric(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	metricID, ok := requireIDParam(w, r, "metricId")
	if !ok {
		return
	}
	existing, err := h.repo.GetMetric(r.Context(), metricID)
	if err != nil || existing.WorkspaceID != workspaceID {
		respondNotFound(w, r, "metric")
		return
	}
	request, ok := decodeJSON[slaMetricRequest](w, r)
	if !ok {
		return
	}
	metric, ok := h.buildMetric(w, r, workspaceID, request)
	if !ok {
		return
	}
	metric.ID = metricID
	ctx := r.Context()
	err = database.WithTx(h.db, func(tx database.Tx) error {
		if err := h.repo.UpdateMetric(ctx, tx, metric); err != nil {
			return err
		}
		if _, err := h.engine.BumpConfigGeneration(ctx, tx, workspaceID); err != nil {
			return err
		}
		return h.enqueueRecalcMetric(ctx, tx, metricID)
	})
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	h.engine.Wake()
	respondJSONOK(w, metric)
}

// DeleteMetric deletes a metric; cycles and jobs cascade.
func (h *SLAHandler) DeleteMetric(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	metricID, ok := requireIDParam(w, r, "metricId")
	if !ok {
		return
	}
	existing, err := h.repo.GetMetric(r.Context(), metricID)
	if err != nil || existing.WorkspaceID != workspaceID {
		respondNotFound(w, r, "metric")
		return
	}
	ctx := r.Context()
	err = database.WithTx(h.db, func(tx database.Tx) error {
		if err := h.repo.DeleteMetric(ctx, tx, metricID); err != nil {
			return err
		}
		_, err := h.engine.BumpConfigGeneration(ctx, tx, workspaceID)
		return err
	})
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, map[string]bool{"deleted": true})
}

// StartRecalculation enqueues a recalculation for one metric or the whole
// workspace.
func (h *SLAHandler) StartRecalculation(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	var request struct {
		MetricID int `json:"metric_id"`
	}
	request, ok = decodeJSON[struct {
		MetricID int `json:"metric_id"`
	}](w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	metricIDs := []int{request.MetricID}
	if request.MetricID == 0 {
		metrics, err := h.repo.ListMetrics(ctx, workspaceID)
		if err != nil {
			respondError(w, r, slaInternal(err))
			return
		}
		metricIDs = metricIDs[:0]
		for _, metric := range metrics {
			metricIDs = append(metricIDs, metric.ID)
		}
	}
	err := database.WithTx(h.db, func(tx database.Tx) error {
		for _, metricID := range metricIDs {
			if err := h.enqueueRecalcMetric(ctx, tx, metricID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	h.engine.Wake()
	respondJSONOK(w, map[string]any{"enqueued": len(metricIDs)})
}

// GetReport returns the completed-cycle compliance report for a workspace.
// It applies the same item-view visibility filter as item listings.
func (h *SLAHandler) GetReport(w http.ResponseWriter, r *http.Request) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return
	}
	workspaceID, ok := requireIDParam(w, r, "id")
	if !ok {
		return
	}
	if !RequireWorkspacePermission(w, r, user.ID, workspaceID, models.PermissionItemView, h.permissionService) {
		return
	}
	from, ok := parseReportTime(w, r, "from")
	if !ok {
		return
	}
	to, ok := parseReportTime(w, r, "to")
	if !ok {
		return
	}
	report, err := h.repo.SLAReport(r.Context(), workspaceID, from, to)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, report)
}

func parseReportTime(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil {
		respondValidationError(w, r, name+" must be RFC3339 or YYYY-MM-DD")
		return nil, false
	}
	return &parsed, true
}

// ListRecalculations reports pending recalculation work for a workspace.
func (h *SLAHandler) ListRecalculations(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.authorizeWorkspaceAdmin(w, r)
	if !ok {
		return
	}
	jobs, err := h.repo.ListRecalculationJobs(r.Context(), workspaceID)
	if err != nil {
		respondError(w, r, slaInternal(err))
		return
	}
	respondJSONOK(w, jobs)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func slaInternal(err error) *restapi.APIError {
	slog.Error("SLA handler internal error", slog.String("component", "sla"), slog.Any("error", err))
	return restapi.ErrInternalError
}

func (h *SLAHandler) authorizeWorkspaceAdmin(w http.ResponseWriter, r *http.Request) (int, bool) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return 0, false
	}
	workspaceID, ok := requireIDParam(w, r, "id")
	if !ok {
		return 0, false
	}
	if !RequireWorkspacePermission(w, r, user.ID, workspaceID, models.PermissionWorkspaceAdmin, h.permissionService) {
		return 0, false
	}
	return workspaceID, true
}

func (h *SLAHandler) authorizeTeamAdmin(w http.ResponseWriter, r *http.Request) (int, bool) {
	user, ok := RequireAuth(w, r)
	if !ok {
		return 0, false
	}
	teamID, ok := requireIDParam(w, r, "id")
	if !ok {
		return 0, false
	}
	isAdmin, err := h.teamRepo.IsTeamAdmin(teamID, user.ID)
	if err != nil || !isAdmin {
		respondNotFound(w, r, "team")
		return 0, false
	}
	return teamID, true
}

func (h *SLAHandler) buildMetric(w http.ResponseWriter, r *http.Request, workspaceID int, request slaMetricRequest) (*models.SLAMetric, bool) {
	name := strings.TrimSpace(request.Name)
	if name == "" {
		respondValidationError(w, r, "name is required")
		return nil, false
	}
	displayFormat := request.DisplayFormat
	if displayFormat == "" {
		displayFormat = "time"
	}
	if displayFormat != "time" && displayFormat != "due_date" {
		respondValidationError(w, r, "display_format must be time or due_date")
		return nil, false
	}
	isActive := true
	if request.IsActive != nil {
		isActive = *request.IsActive
	}
	importStatus := request.ImportStatus
	if importStatus == "" {
		importStatus = "native"
	}
	metric := &models.SLAMetric{
		WorkspaceID:   workspaceID,
		Name:          name,
		DisplayFormat: displayFormat,
		Position:      request.Position,
		IsActive:      isActive,
		ImportStatus:  importStatus,
		Conditions:    request.Conditions,
		Goals:         request.Goals,
	}
	if err := h.validateConditions(metric.Conditions); err != nil {
		respondValidationError(w, r, err.Error())
		return nil, false
	}
	if err := h.validateTargets(r.Context(), workspaceID, metric.Goals); err != nil {
		respondValidationError(w, r, err.Error())
		return nil, false
	}
	return metric, true
}

func (h *SLAHandler) validateConditions(conditions []models.SLACondition) error {
	for i := range conditions {
		condition := &conditions[i]
		switch condition.Phase {
		case models.SLAPhaseStart, models.SLAPhasePause, models.SLAPhaseStop:
		default:
			return errors.New("condition phase must be start, pause, or stop")
		}
		if strings.TrimSpace(condition.ConditionType) == "" {
			return errors.New("condition_type is required")
		}
	}
	return nil
}

// validateTargets ensures every referenced calendar belongs to the workspace or
// to a team bound to it. A promise may never depend on an unauthorized
// calendar.
func (h *SLAHandler) validateTargets(ctx context.Context, workspaceID int, goals []models.SLAGoal) error {
	for i := range goals {
		for j := range goals[i].Targets {
			target := &goals[i].Targets[j]
			if target.CalendarID == 0 {
				return errors.New("each goal target needs a calendar_id")
			}
			if target.TargetMs <= 0 {
				return errors.New("each goal target needs a positive target_ms")
			}
			accessible, err := h.calendarAccessible(ctx, workspaceID, target.CalendarID)
			if err != nil {
				return err
			}
			if !accessible {
				return errors.New("goal target references a calendar this workspace cannot use")
			}
		}
	}
	return nil
}

func (h *SLAHandler) calendarAccessible(ctx context.Context, workspaceID, calendarID int) (bool, error) {
	var accessible bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM working_calendars c
		WHERE c.id = ?
		  AND (c.workspace_id = ?
		       OR c.team_id IN (SELECT team_id FROM team_workspace_bindings WHERE workspace_id = ?))
	)`, calendarID, workspaceID, workspaceID).Scan(&accessible)
	if err != nil {
		return false, err
	}
	return accessible, nil
}

func (h *SLAHandler) enqueueRecalcMetric(ctx context.Context, tx database.Tx, metricID int) error {
	metric := metricID
	return h.repo.UpsertJob(ctx, tx, &models.SLAJob{Kind: models.SLAJobRecalcMetric, MetricID: &metric, DueAt: time.Now().UTC()})
}
