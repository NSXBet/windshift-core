//go:build !noplugins

package plugins

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"windshift/internal/itemevents"
	"windshift/internal/services"
	"windshift/internal/utils"

	extism "github.com/extism/go-sdk"
)

// buildHostFunctions creates all host functions available to plugins.
func (m *Manager) buildHostFunctions() []extism.HostFunction {
	return []extism.HostFunction{
		extism.NewHostFunctionWithStack("log", m.logHostFunction, []extism.ValueType{extism.ValueTypeI64}, nil),
		extism.NewHostFunctionWithStack("smtp_send", m.smtpHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("http_fetch", m.httpFetchHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("cli_exec", m.cliExecHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("kv_get", m.kvGetHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("kv_set", m.kvSetHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("kv_delete", m.kvDeleteHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("create_comment", m.createCommentHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("scm_create_branch", m.scmCreateBranchHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("scm_create_item_link", m.scmCreateItemLinkHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("item_upsert", m.itemUpsertHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("item_lookup", m.itemLookupHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("workspace_upsert", m.workspaceUpsertHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("member_upsert", m.memberUpsertHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("iteration_upsert", m.iterationUpsertHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
		extism.NewHostFunctionWithStack("purge_sync", m.purgeSyncHostFunction, []extism.ValueType{extism.ValueTypeI64}, []extism.ValueType{extism.ValueTypeI64}),
	}
}

func (m *Manager) logHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("log host function failed to read payload", "error", err)
		return
	}

	var logReq LogRequest
	if err := json.Unmarshal(payload, &logReq); err != nil {
		m.logger.Warn("log host function failed to parse payload", "error", err)
		return
	}

	level := slog.LevelInfo
	switch strings.ToLower(logReq.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "info":
		level = slog.LevelInfo
	}

	m.logger.Log(ctx, level, logReq.Message)
}

func (m *Manager) smtpHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("smtp_send host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var sendReq SMTPSendRequest
	if err := json.Unmarshal(payload, &sendReq); err != nil {
		m.logger.Warn("smtp_send host function failed to parse payload", "error", err)
		stack[0] = 0
		return
	}

	result := SMTPSendResponse{Status: "ok"}
	if m.smtpSender == nil {
		result.Status = "error"
		result.Error = "smtp sender not configured"
	} else if err := m.smtpSender.Send(ctx, sendReq); err != nil {
		result.Status = "error"
		result.Error = err.Error()
	}

	m.writeHostResponse(plugin, stack, result)
}

func (m *Manager) httpFetchHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("http_fetch host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var fetchReq HTTPFetchRequest
	if err = json.Unmarshal(payload, &fetchReq); err != nil {
		m.logger.Warn("http_fetch host function failed to parse payload", "error", err)
		stack[0] = 0
		return
	}

	method := strings.ToUpper(fetchReq.Method)
	if method == "" {
		method = http.MethodGet
	}

	if fetchReq.URL == "" {
		m.writeHostResponse(plugin, stack, HTTPFetchResponse{Status: http.StatusBadRequest})
		return
	}

	client := m.httpClient
	if client == nil {
		client = utils.NewHTTPClient(10 * time.Second)
	}

	timeout := m.pluginTimeout
	if fetchReq.TimeoutMs > 0 {
		timeout = time.Duration(fetchReq.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, fetchReq.URL, bytes.NewReader(fetchReq.Body))
	if err != nil {
		m.writeHostResponse(plugin, stack, HTTPFetchResponse{Status: http.StatusBadRequest, Body: []byte(err.Error())})
		return
	}

	for k, v := range fetchReq.Headers {
		req.Header.Set(k, v)
	}

	fetchStart := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		m.writeHostResponse(plugin, stack, HTTPFetchResponse{Status: http.StatusBadGateway, Body: []byte(err.Error())})
		return
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		m.writeHostResponse(plugin, stack, HTTPFetchResponse{Status: http.StatusBadGateway, Body: []byte(readErr.Error())})
		return
	}
	headers := make(map[string]string)
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			headers[k] = vals[0]
		}
	}

	m.logger.Info("http_fetch done", "url", fetchReq.URL, "elapsed", time.Since(fetchStart).String(), "err", err != nil, "bytes", len(body))
	m.writeHostResponse(plugin, stack, HTTPFetchResponse{
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    body,
	})
}

func (m *Manager) writeHostResponse(plugin *extism.CurrentPlugin, stack []uint64, payload any) {
	t0 := time.Now()
	data, err := json.Marshal(payload)
	if err != nil {
		m.logger.Warn("host response marshal failed", "error", err)
		stack[0] = 0
		return
	}

	ptr, err := plugin.WriteBytes(data)
	m.logger.Info("writeHostResponse", "bytes", len(data), "elapsed", time.Since(t0).String(), "err", err != nil)
	if err != nil {
		m.logger.Warn("host response write failed", "error", err)
		stack[0] = 0
		return
	}

	stack[0] = ptr
}

func (m *Manager) kvGetHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("kv_get host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var kvReq KVGetRequest
	if err = json.Unmarshal(payload, &kvReq); err != nil {
		m.logger.Warn("kv_get host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, KVGetResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if kvReq.Key == "" {
		m.writeHostResponse(plugin, stack, KVGetResponse{Status: "error", Error: "key is required"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if pluginName == "" {
		m.writeHostResponse(plugin, stack, KVGetResponse{Status: "error", Error: "no plugin context"})
		return
	}

	if m.db == nil {
		m.writeHostResponse(plugin, stack, KVGetResponse{Status: "error", Error: "database not configured"})
		return
	}

	kvStart := time.Now()
	var value string
	err = m.db.QueryRowContext(ctx,
		"SELECT value FROM plugin_kv_store WHERE plugin_name = ? AND key = ?",
		pluginName, kvReq.Key,
	).Scan(&value)

	if errors.Is(err, sql.ErrNoRows) {
		m.writeHostResponse(plugin, stack, KVGetResponse{Status: "not_found"})
		return
	}
	if err != nil {
		m.logger.Warn("kv_get database error", "error", err, "plugin", pluginName, "key", kvReq.Key)
		m.writeHostResponse(plugin, stack, KVGetResponse{Status: "error", Error: "database error"})
		return
	}

	m.writeHostResponse(plugin, stack, KVGetResponse{Status: "ok", Value: value})
	if d := time.Since(kvStart); d > 100*time.Millisecond {
		m.logger.Warn("kv_get slow", "key", kvReq.Key, "elapsed", d.String())
	}
}

func (m *Manager) kvSetHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("kv_set host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var kvReq KVSetRequest
	if err = json.Unmarshal(payload, &kvReq); err != nil {
		m.logger.Warn("kv_set host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, KVSetResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if kvReq.Key == "" {
		m.writeHostResponse(plugin, stack, KVSetResponse{Status: "error", Error: "key is required"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if pluginName == "" {
		m.writeHostResponse(plugin, stack, KVSetResponse{Status: "error", Error: "no plugin context"})
		return
	}

	if m.db == nil {
		m.writeHostResponse(plugin, stack, KVSetResponse{Status: "error", Error: "database not configured"})
		return
	}

	now := time.Now()
	// Use upsert pattern - INSERT ... ON CONFLICT UPDATE
	var query string
	if m.db.GetDriverName() == "postgres" {
		query = `
			INSERT INTO plugin_kv_store (plugin_name, key, value, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (plugin_name, key) DO UPDATE SET value = $3, updated_at = $5
		`
	} else {
		query = `
			INSERT INTO plugin_kv_store (plugin_name, key, value, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(plugin_name, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`
	}

	_, err = m.db.ExecWriteContext(ctx, query, pluginName, kvReq.Key, kvReq.Value, now, now)
	if err != nil {
		m.logger.Warn("kv_set database error", "error", err, "plugin", pluginName, "key", kvReq.Key)
		m.writeHostResponse(plugin, stack, KVSetResponse{Status: "error", Error: "database error"})
		return
	}

	m.writeHostResponse(plugin, stack, KVSetResponse{Status: "ok"})
}

func (m *Manager) kvDeleteHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("kv_delete host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var kvReq KVDeleteRequest
	if err = json.Unmarshal(payload, &kvReq); err != nil {
		m.logger.Warn("kv_delete host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, KVDeleteResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if kvReq.Key == "" {
		m.writeHostResponse(plugin, stack, KVDeleteResponse{Status: "error", Error: "key is required"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if pluginName == "" {
		m.writeHostResponse(plugin, stack, KVDeleteResponse{Status: "error", Error: "no plugin context"})
		return
	}

	if m.db == nil {
		m.writeHostResponse(plugin, stack, KVDeleteResponse{Status: "error", Error: "database not configured"})
		return
	}

	_, err = m.db.ExecWriteContext(ctx,
		"DELETE FROM plugin_kv_store WHERE plugin_name = ? AND key = ?",
		pluginName, kvReq.Key,
	)
	if err != nil {
		m.logger.Warn("kv_delete database error", "error", err, "plugin", pluginName, "key", kvReq.Key)
		m.writeHostResponse(plugin, stack, KVDeleteResponse{Status: "error", Error: "database error"})
		return
	}

	m.writeHostResponse(plugin, stack, KVDeleteResponse{Status: "ok"})
}

func (m *Manager) createCommentHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("create_comment host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var req CreateCommentRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("create_comment host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if req.ItemID <= 0 {
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "item_id is required"})
		return
	}
	if req.AuthorID <= 0 {
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "author_id is required"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "content is required"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if pluginName == "" {
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "no plugin context"})
		return
	}

	if m.commentService == nil {
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "comment service not configured"})
		return
	}

	result, err := m.commentService.Create(services.CreateCommentParams{
		ItemID:                req.ItemID,
		AuthorID:              req.AuthorID,
		Content:               req.Content,
		ActorUserID:           req.AuthorID,
		SuppressNotifications: req.SuppressNotifications,
		EventMetadata:         itemevents.Integration(pluginName, "plugin"),
	})
	if err != nil {
		m.logger.Warn("create_comment failed", "error", err, "plugin", pluginName, "item_id", req.ItemID)
		m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "error", Error: "failed to create comment"})
		return
	}

	m.logger.Info("plugin created comment", "plugin", pluginName, "comment_id", result.CommentID, "item_id", req.ItemID)
	m.writeHostResponse(plugin, stack, CreateCommentResponse{Status: "ok", CommentID: int(result.CommentID)})
}

func (m *Manager) scmCreateBranchHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("scm_create_branch host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var req SCMCreateBranchRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("scm_create_branch host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if req.WorkspaceRepositoryID <= 0 {
		m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "error", Error: "workspace_repository_id is required"})
		return
	}
	if strings.TrimSpace(req.BranchName) == "" {
		m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "error", Error: "branch_name is required"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if pluginName == "" {
		m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "error", Error: "no plugin context"})
		return
	}

	if m.scmService == nil {
		m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "error", Error: "SCM service not configured"})
		return
	}

	branchURL, err := m.scmService.CreateBranchForRepository(ctx, req.WorkspaceRepositoryID, req.BranchName, req.BaseBranch)
	if err != nil {
		m.logger.Warn("scm_create_branch failed", "error", err, "plugin", pluginName, "repo_id", req.WorkspaceRepositoryID)
		m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "error", Error: err.Error()})
		return
	}

	m.logger.Info("plugin created branch", "plugin", pluginName, "repo_id", req.WorkspaceRepositoryID, "branch", req.BranchName)
	m.writeHostResponse(plugin, stack, SCMCreateBranchResponse{Status: "ok", BranchURL: branchURL})
}

func (m *Manager) scmCreateItemLinkHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("scm_create_item_link host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var req SCMCreateItemLinkRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("scm_create_item_link host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if req.ItemID <= 0 {
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "item_id is required"})
		return
	}
	if req.WorkspaceRepositoryID <= 0 {
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "workspace_repository_id is required"})
		return
	}
	if strings.TrimSpace(req.LinkType) == "" {
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "link_type is required"})
		return
	}
	if strings.TrimSpace(req.ExternalID) == "" {
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "external_id is required"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if pluginName == "" {
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "no plugin context"})
		return
	}

	if m.scmService == nil {
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: "SCM service not configured"})
		return
	}

	linkID, err := m.scmService.CreateItemSCMLink(ctx, req.ItemID, req.WorkspaceRepositoryID, req.LinkType, req.ExternalID, req.ExternalURL, req.Title)
	if err != nil {
		m.logger.Warn("scm_create_item_link failed", "error", err, "plugin", pluginName, "item_id", req.ItemID)
		m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "error", Error: err.Error()})
		return
	}

	m.logger.Info("plugin created item SCM link", "plugin", pluginName, "item_id", req.ItemID, "link_id", linkID)
	m.writeHostResponse(plugin, stack, SCMCreateItemLinkResponse{Status: "ok", LinkID: linkID})
}

func (m *Manager) itemUpsertHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("item_upsert host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var req ItemUpsertRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("item_upsert host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, ItemUpsertResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	pluginName := pluginNameFromContext(ctx)
	if m.db == nil {
		m.writeHostResponse(plugin, stack, ItemUpsertResponse{Status: "error", Error: "database not configured"})
		return
	}

	result, err := services.NewShortcutSyncService(m.db).WithPermissionChecker(m.permChecker).Upsert(ctx, services.ShortcutItemUpsertRequest{
		ExternalKind:        req.ExternalKind,
		ExternalID:          req.ExternalID,
		ExternalURL:         req.ExternalURL,
		ExternalUpdatedAt:   req.ExternalUpdatedAt,
		WorkspaceID:         req.WorkspaceID,
		Title:               req.Title,
		Description:         req.Description,
		StatusName:          req.StatusName,
		ItemTypeName:        req.ItemTypeName,
		PriorityName:        req.PriorityName,
		ProjectName:         req.ProjectName,
		DueDate:             req.DueDate,
		StoryPoints:         req.StoryPoints,
		Labels:              req.Labels,
		LabelMode:           req.LabelMode,
		ParentExternalKind:  req.ParentExternalKind,
		ParentExternalID:    req.ParentExternalID,
		AssigneeExternalID:  req.AssigneeExternalID,
		IterationExternalID: req.IterationExternalID,
	})
	if err != nil {
		m.logger.Warn("item_upsert failed", "error", err, "plugin", pluginName, "external_kind", req.ExternalKind, "external_id", req.ExternalID)
		m.writeHostResponse(plugin, stack, ItemUpsertResponse{Status: "error", Error: err.Error()})
		return
	}

	m.writeHostResponse(plugin, stack, ItemUpsertResponse{
		Status:  "ok",
		ItemID:  strconv.Itoa(result.ItemID),
		ItemKey: result.ItemKey,
		Created: result.Created,
	})
}

func (m *Manager) itemLookupHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("item_lookup host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	var req ItemLookupRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("item_lookup host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, ItemLookupResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if m.db == nil {
		m.writeHostResponse(plugin, stack, ItemLookupResponse{Status: "error", Error: "database not configured"})
		return
	}

	result, err := services.NewShortcutSyncService(m.db).Lookup(ctx, req.ExternalKind, req.ExternalID)
	if err != nil {
		m.logger.Warn("item_lookup failed", "error", err, "plugin", pluginNameFromContext(ctx), "external_kind", req.ExternalKind, "external_id", req.ExternalID)
		m.writeHostResponse(plugin, stack, ItemLookupResponse{Status: "error", Error: err.Error()})
		return
	}
	if result == nil {
		m.writeHostResponse(plugin, stack, ItemLookupResponse{Status: "ok"})
		return
	}

	resp := ItemLookupResponse{
		Status:  "ok",
		Found:   true,
		ItemID:  strconv.Itoa(result.ItemID),
		ItemKey: result.ItemKey,
	}
	if result.ExternalUpdatedAt != nil {
		resp.ExternalUpdatedAt = result.ExternalUpdatedAt.UTC().Format(time.RFC3339)
	}
	if result.LastSyncedAt != nil {
		resp.LastSyncedAt = result.LastSyncedAt.UTC().Format(time.RFC3339)
	}
	m.writeHostResponse(plugin, stack, resp)
}

// purgeSyncHostFunction wipes everything the calling plugin ever synced:
// item mappings (items cascade), plugin KV state, plugin-created workspaces,
// team workflows/configuration sets and the statuses the sync minted. The
// request payload is unused; the empty object keeps the pointer ABI uniform.
func (m *Manager) purgeSyncHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	if _, err := plugin.ReadBytes(stack[0]); err != nil {
		m.logger.Warn("purge_sync host function failed to read payload", "error", err)
		stack[0] = 0
		return
	}

	if m.db == nil {
		m.writeHostResponse(plugin, stack, PurgeSyncResponse{Status: "error", Error: "database not configured"})
		return
	}

	result, err := services.NewShortcutSyncService(m.db).PurgeSync(ctx, pluginNameFromContext(ctx))
	if err != nil {
		m.logger.Warn("purge_sync failed", "error", err, "plugin", pluginNameFromContext(ctx))
		m.writeHostResponse(plugin, stack, PurgeSyncResponse{Status: "error", Error: err.Error()})
		return
	}
	m.writeHostResponse(plugin, stack, PurgeSyncResponse{Status: "ok", Result: &result})
}

func (m *Manager) workspaceUpsertHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("workspace_upsert host function failed to read payload", "error", err)
		m.writeHostResponse(plugin, stack, WorkspaceUpsertResponse{Status: "error", Error: "failed to read payload"})
		return
	}

	var req WorkspaceUpsertRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("workspace_upsert host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, WorkspaceUpsertResponse{Status: "error", Error: "invalid request payload"})
		return
	}

	if m.db == nil {
		m.writeHostResponse(plugin, stack, WorkspaceUpsertResponse{Status: "error", Error: "database not configured"})
		return
	}

	result, err := services.NewShortcutSyncService(m.db).UpsertWorkspace(ctx, services.ShortcutWorkspaceUpsertRequest{
		ExternalID:   req.ExternalID,
		ExternalKind: req.ExternalKind,
		Name:         req.Name,
		CreatorID:    req.CreatorID,
		States:       req.States,
	})
	if err != nil {
		m.logger.Warn("workspace_upsert failed", "error", err, "plugin", pluginNameFromContext(ctx), "external_id", req.ExternalID)
		m.writeHostResponse(plugin, stack, WorkspaceUpsertResponse{Status: "error", Error: err.Error()})
		return
	}
	m.writeHostResponse(plugin, stack, WorkspaceUpsertResponse{
		Status:      "ok",
		WorkspaceID: strconv.Itoa(result.WorkspaceID),
		Created:     result.Created,
	})
}

// memberUpsertHostFunction imports one Shortcut member as a windshift user so
// a story's assignee slot can point at a real account.
func (m *Manager) memberUpsertHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("member_upsert host function failed to read payload", "error", err)
		m.writeHostResponse(plugin, stack, MemberUpsertResponse{Status: "error", Error: "failed to read payload"})
		return
	}

	var req MemberUpsertRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("member_upsert host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, MemberUpsertResponse{Status: "error", Error: err.Error()})
		return
	}
	if m.db == nil {
		m.writeHostResponse(plugin, stack, MemberUpsertResponse{Status: "error", Error: "database not configured"})
		return
	}

	result, err := services.NewShortcutSyncService(m.db).UpsertMember(ctx, services.ShortcutMemberUpsertRequest{
		ExternalID:   req.ExternalID,
		Name:         req.Name,
		Email:        req.Email,
		UsernameHint: req.Username,
	})
	if err != nil {
		m.logger.Warn("member_upsert failed", "error", err, "plugin", pluginNameFromContext(ctx), "external_id", req.ExternalID)
		m.writeHostResponse(plugin, stack, MemberUpsertResponse{Status: "error", Error: err.Error()})
		return
	}
	m.writeHostResponse(plugin, stack, MemberUpsertResponse{
		Status:  "ok",
		UserID:  strconv.Itoa(result.UserID),
		Created: result.Created,
	})
}

// iterationUpsertHostFunction imports one Shortcut iteration into a workspace
// so stories can carry their sprint.
func (m *Manager) iterationUpsertHostFunction(ctx context.Context, plugin *extism.CurrentPlugin, stack []uint64) {
	payload, err := plugin.ReadBytes(stack[0])
	if err != nil {
		m.logger.Warn("iteration_upsert host function failed to read payload", "error", err)
		m.writeHostResponse(plugin, stack, IterationUpsertResponse{Status: "error", Error: "failed to read payload"})
		return
	}

	var req IterationUpsertRequest
	if err = json.Unmarshal(payload, &req); err != nil {
		m.logger.Warn("iteration_upsert host function failed to parse payload", "error", err)
		m.writeHostResponse(plugin, stack, IterationUpsertResponse{Status: "error", Error: err.Error()})
		return
	}
	if m.db == nil {
		m.writeHostResponse(plugin, stack, IterationUpsertResponse{Status: "error", Error: "database not configured"})
		return
	}

	result, err := services.NewShortcutSyncService(m.db).UpsertIteration(ctx, services.ShortcutIterationUpsertRequest{
		ExternalID: req.ExternalID,
		Name:       req.Name,
		StartDate:  req.StartDate,
		EndDate:    req.EndDate,
		Status:     req.Status,
		Workspace:  req.Workspace,
	})
	if err != nil {
		m.logger.Warn("iteration_upsert failed", "error", err, "plugin", pluginNameFromContext(ctx), "external_id", req.ExternalID)
		m.writeHostResponse(plugin, stack, IterationUpsertResponse{Status: "error", Error: err.Error()})
		return
	}
	m.writeHostResponse(plugin, stack, IterationUpsertResponse{
		Status:      "ok",
		IterationID: strconv.Itoa(result.IterationID),
		Created:     result.Created,
	})
}
