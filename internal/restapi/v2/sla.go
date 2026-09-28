package v2

import (
	"net/http"
	"strings"
	"time"

	"windshift/internal/models"
)

// registerSLARoutes publishes the read-only SLA state on the canonical v2
// surface. Portal customers receive nothing; the routes require items:read and
// the same workspace visibility check as item reads.
func registerSLARoutes(builder *routeBuilder, deps Deps) {
	builder.Read("/items/{item_id}/sla", AuthAuthenticated, []string{"items:read"}, itemSLA(deps))
	builder.Read("/workspaces/{workspace_id}/sla/report", AuthAuthenticated, []string{"items:read"}, workspaceSLAReport(deps))
}

func itemSLA(deps Deps) readOperation[[]models.ItemSLA] {
	return func(r *http.Request) ([]models.ItemSLA, error) {
		if deps.SLA == nil {
			return nil, newError(http.StatusNotFound, "not_found", "Item was not found")
		}
		item, err := requireItem(r, deps, deps.Access.CanViewWorkspace)
		if err != nil {
			return nil, err
		}
		states, err := deps.SLA.ItemSLA(r.Context(), item.ID, item.WorkspaceID)
		if err != nil {
			return nil, internalError(err)
		}
		return states, nil
	}
}

func workspaceSLAReport(deps Deps) readOperation[models.SLAReport] {
	return func(r *http.Request) (models.SLAReport, error) {
		if deps.SLA == nil {
			return models.SLAReport{}, newError(http.StatusNotFound, "not_found", "Workspace was not found")
		}
		workspaceID, err := pathID(r, "workspace_id")
		if err != nil {
			return models.SLAReport{}, err
		}
		user, err := principal(r)
		if err != nil {
			return models.SLAReport{}, err
		}
		if err := requireWorkspace(deps.Access.CanViewWorkspace, user.ID, workspaceID); err != nil {
			return models.SLAReport{}, err
		}
		from, err := querySLATime(r, "from")
		if err != nil {
			return models.SLAReport{}, err
		}
		to, err := querySLATime(r, "to")
		if err != nil {
			return models.SLAReport{}, err
		}
		report, err := deps.SLA.Report(r.Context(), workspaceID, from, to)
		if err != nil {
			return models.SLAReport{}, internalError(err)
		}
		return *report, nil
	}
}

func querySLATime(r *http.Request, name string) (*time.Time, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil {
		return nil, newError(http.StatusBadRequest, "invalid_input", name+" must be RFC3339 or YYYY-MM-DD")
	}
	return &parsed, nil
}
