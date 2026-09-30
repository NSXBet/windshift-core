package services

import (
	"context"
	"fmt"

	"windshift/internal/models"
	"windshift/internal/repository"
)

// SupportQueueDefinition is one stable support-queue preset. The CQL runs
// through the same item-list pipeline as every other list, so queue counts
// and rows enforce workspace visibility identically to the items API.
type SupportQueueDefinition struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	QL          string `json:"ql"`
	Count       int64  `json:"count"`
}

// supportQueuePresets is the ordered catalog of queue presets. Keys are a
// stable product contract (clients switch queues by key); the CQL is the
// source of truth for each queue's membership.
//
// Waiting means the ticket's SLA cycle is paused — the WI-584 pause rules
// fire exactly when support waits on the customer. Workspaces without SLA
// configuration therefore see an empty waiting queue rather than a wrong one.
var supportQueuePresets = []struct {
	Key         string
	Name        string
	Description string
	QL          string
}{
	{
		Key:         "unassigned",
		Name:        "Unassigned",
		Description: "Open tickets with no team and no assignee — the routing fallback",
		QL:          `assignee IS NULL AND team IS NULL AND statusCompleted = false`,
	},
	{
		Key:         "team-owned",
		Name:        "Team-owned",
		Description: "Open tickets a team owns that no agent has picked up",
		QL:          `team IS NOT NULL AND assignee IS NULL AND statusCompleted = false`,
	},
	{
		Key:         "assigned-to-me",
		Name:        "Assigned to me",
		Description: "Open tickets assigned to the current agent",
		QL:          `assignee = currentUser() AND statusCompleted = false`,
	},
	{
		Key:         "waiting",
		Name:        "Waiting",
		Description: "Open tickets whose SLA is paused — support waits on the customer",
		QL:          `slaPaused = true AND statusCompleted = false`,
	},
	{
		Key:         "overdue",
		Name:        "Overdue",
		Description: "Open tickets past their due date",
		QL:          `dueDate < startofday() AND statusCompleted = false`,
	},
	{
		Key:         "recently-updated",
		Name:        "Recently updated",
		Description: "Open tickets with activity in the last 24 hours",
		QL:          `updatedAt > -1d AND statusCompleted = false`,
	},
	{
		Key:         "sla-at-risk",
		Name:        "SLA at risk",
		Description: "Open tickets whose running SLA deadline is within 2 days or already passed",
		QL:          `slaRunning = true AND slaDeadline <= 2d`,
	},
}

// SupportQueues resolves a workspace by key and returns the queue catalog
// with live, visibility-filtered counts for the requesting user. Inactive
// workspaces answer 404 unless the caller is a workspace admin, matching the
// item read contract.
func (s *ItemApplicationService) SupportQueues(ctx context.Context, userID int, workspaceKey string) ([]SupportQueueDefinition, error) {
	workspaceID, err := repository.NewWorkspaceRepository(s.db).FindIDByKey(workspaceKey)
	if err != nil {
		return nil, err
	}
	workspace, err := repository.NewWorkspaceRepository(s.db).FindByIDBasic(workspaceID)
	if err != nil {
		return nil, err
	}
	accessible, err := s.perm.AccessibleWorkspaceIDs(userID)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, id := range accessible {
		if id == workspaceID {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, repository.ErrNotFound
	}
	if !workspace.Active {
		isAdmin, err := s.perm.HasWorkspacePermission(userID, workspaceID, models.PermissionWorkspaceAdmin)
		if err != nil {
			return nil, err
		}
		if !isAdmin {
			return nil, repository.ErrNotFound
		}
	}

	queues := make([]SupportQueueDefinition, 0, len(supportQueuePresets))
	for _, preset := range supportQueuePresets {
		count, err := s.queueCount(ctx, userID, workspaceID, preset.QL)
		if err != nil {
			return nil, fmt.Errorf("count queue %s: %w", preset.Key, err)
		}
		queues = append(queues, SupportQueueDefinition{
			Key:         preset.Key,
			Name:        preset.Name,
			Description: preset.Description,
			QL:          preset.QL,
			Count:       count,
		})
	}
	return queues, nil
}

// queueCount evaluates one queue's CQL through the shared list path and
// returns the total matching visible tickets.
func (s *ItemApplicationService) queueCount(ctx context.Context, userID, workspaceID int, ql string) (int64, error) {
	items, total, err := s.crud.ListWithQLContext(ctx, ListWithQLParams{
		WorkspaceID:  workspaceID,
		QLQuery:      ql,
		WorkspaceIDs: []int{workspaceID},
		UserID:       userID,
		Pagination:   repository.PaginationParams{Page: 1, Limit: 1},
	})
	if err != nil {
		return 0, err
	}
	_ = items
	return int64(total), nil
}
