<script>
  import { onMount } from 'svelte';
  import { t } from '../../stores/i18n.svelte.js';
  import { api } from '../../api.js';
import { fetchV2Data } from '../../api/core.js';
  import { errorToast, successToast } from '../../stores/toasts.svelte.js';
  import { authStore, workspaceDataStore } from '../../stores';
  import { navigate } from '../../router.js';
  import ViewHeader from '../../layout/ViewHeader.svelte';
  import EmptyState from '../../components/EmptyState.svelte';
  import Spinner from '../../components/Spinner.svelte';
  import { formatDateTimeLocale, formatDate } from '../../utils/dateFormatter.js';

  let { workspaceId, queue = null } = $props();

  // Queue catalog from the backend (stable keys, live counts, queue CQL).
  let queues = $state([]);
  let activeQueueKey = $state(queue || 'unassigned');
  let loadingQueues = $state(true);

  // Ticket rows for the active queue.
  let items = $state([]);
  let loadingItems = $state(false);
  let itemsTruncated = $state(false);

  // Bulk selection state.
  let selectedIds = $state(new Set());
  let bulkRunning = $state(false);
  let bulkTeamId = $state('');
  let teams = $state([]);

  let workspace = $derived(workspaceDataStore.workspace);
  let users = $derived(workspaceDataStore.users ?? []);
  let statuses = $derived(workspaceDataStore.statuses ?? []);
  let priorities = $derived(workspaceDataStore.priorities ?? []);
  let activeQueue = $derived(queues.find((entry) => entry.key === activeQueueKey) ?? null);
  let activeQueueCount = $derived(activeQueue?.count ?? 0);
  let allSelected = $derived(items.length > 0 && items.every((item) => selectedIds.has(item.id)));
  let currentUser = $derived(authStore.currentUser);

  const QUEUE_PAGE_SIZE = 100;

  function statusName(statusId) {
    return statuses.find((status) => status.id === statusId)?.name ?? '';
  }

  function statusColor(statusId) {
    return statuses.find((status) => status.id === statusId)?.color ?? '';
  }

  function priorityName(priorityId) {
    return priorities.find((priority) => priority.id === priorityId)?.name ?? '';
  }

  function ownerLabel(item) {
    if (item.assignee_id != null) {
      const user = users.find((candidate) => candidate.id === item.assignee_id);
      return user?.name ?? `#${item.assignee_id}`;
    }
    return '';
  }

  function formatDueDate(value) {
    if (!value) return '';
    return formatDate(value);
  }

  function formatUpdatedAt(value) {
    if (!value) return '';
    return formatDateTimeLocale(value);
  }

  async function loadQueues() {
    try {
      // Resolve the key directly: the workspace store may still be
      // initializing for a different workspace when this view deep-links in.
      const list = (await api.workspaces.getAll()) ?? [];
      const key = list.find((entry) => entry.id === Number(workspaceId))?.key;
      if (!key) return;
      queues = (await fetchV2Data(`/workspaces/${key}/queues`)) ?? [];
    } catch (error) {
      errorToast(t('supportQueue.loadFailed'));
    }
  }

  async function loadItems() {
    if (!activeQueue?.ql) {
      items = [];
      return;
    }
    loadingItems = true;
    try {
      const query = new URLSearchParams({
        workspace_id: String(workspaceId),
        ql: activeQueue.ql,
        fields: 'summary',
        page_size: String(QUEUE_PAGE_SIZE),
        sort: '-updated_at',
      });
      items = (await fetchV2Data(`/items?${query.toString()}`)) ?? [];
      itemsTruncated = activeQueueCount > items.length;
    } catch (error) {
      items = [];
      errorToast(t('supportQueue.loadFailed'));
    } finally {
      loadingItems = false;
    }
  }

  function selectQueue(key) {
    if (key === activeQueueKey) return;
    activeQueueKey = key;
    selectedIds = new Set();
    history.replaceState(null, '', `/workspaces/${workspaceId}/queue?queue=${encodeURIComponent(key)}`);
    void loadItems();
  }

  function toggleItem(id) {
    const next = new Set(selectedIds);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    selectedIds = next;
  }

  function toggleAll() {
    selectedIds = allSelected ? new Set() : new Set(items.map((item) => item.id));
  }

  function clearSelection() {
    selectedIds = new Set();
  }

  async function runBulkUpdate(fields) {
    const ids = [...selectedIds];
    if (ids.length === 0 || bulkRunning) return;
    bulkRunning = true;
    try {
      await api.items.bulkUpdate(ids, fields);
      successToast(t('supportQueue.bulkSuccess', { n: ids.length }));
      clearSelection();
      await Promise.all([loadQueues(), loadItems()]);
    } catch (error) {
      errorToast(error?.message || t('supportQueue.loadFailed'));
    } finally {
      bulkRunning = false;
    }
  }

  function bulkAssignToMe() {
    if (currentUser?.id == null) return;
    void runBulkUpdate({ assignee_id: currentUser.id });
  }

  function bulkAssignToTeam() {
    if (!bulkTeamId) return;
    void runBulkUpdate({ team_id: Number(bulkTeamId) });
  }

  onMount(() => {
    void loadQueues().then(loadItems);
    void workspaceDataStore.initialize(workspaceId);
    api.teams
      .getAll()
      .then((list) => {
        teams = list ?? [];
      })
      .catch(() => {
        teams = [];
      });
  });
</script>

<div class="p-6" style="background-color: var(--ds-surface); min-height: 100%;">
  <ViewHeader viewName={t('supportQueue.title')} itemCount={activeQueueCount} />

  {#if loadingQueues}
    <div class="flex items-center gap-2 py-8" data-testid="support-queue-loading">
      <Spinner class="w-4 h-4" />
      <span class="text-ds-text-subtle">{t('supportQueue.loading')}</span>
    </div>
  {:else}
    <div class="flex flex-wrap gap-2 mb-4" role="tablist" data-testid="support-queue-tabs">
      {#each queues as entry (entry.key)}
        <button
          type="button"
          role="tab"
          aria-selected={entry.key === activeQueueKey}
          class="px-3 py-1.5 rounded-md text-sm border transition-colors"
          class:border-transparent={entry.key !== activeQueueKey}
          style={
            entry.key === activeQueueKey
              ? 'background-color: var(--ds-accent); color: var(--ds-text-on-accent, #fff); border-color: var(--ds-accent);'
              : 'background-color: var(--ds-background-neutral); color: var(--ds-text); border-color: var(--ds-border);'
          }
          data-testid={`support-queue-tab-${entry.key}`}
          onclick={() => selectQueue(entry.key)}
        >
          {entry.name}
          <span
            class="ml-1.5 inline-block rounded-full text-xs px-1.5"
            style="background-color: var(--ds-background-neutral-strong, rgba(0,0,0,0.08));"
            data-testid={`support-queue-count-${entry.key}`}
          >
            {entry.count}
          </span>
        </button>
      {/each}
    </div>

    {#if selectedIds.size > 0}
      <div
        class="flex flex-wrap items-center gap-2 mb-3 px-3 py-2 rounded-md border"
        style="border-color: var(--ds-border); background-color: var(--ds-background-neutral);"
        data-testid="support-queue-bulk-bar"
      >
        <span class="text-sm text-ds-text">{t('supportQueue.bulkBar', { n: selectedIds.size })}</span>
        <button
          type="button"
          class="px-2.5 py-1 rounded text-sm border"
          style="border-color: var(--ds-border);"
          data-testid="support-queue-bulk-assign-me"
          disabled={bulkRunning}
          onclick={bulkAssignToMe}
        >
          {t('supportQueue.bulkAssignMe')}
        </button>
        <select
          class="px-2 py-1 rounded text-sm border"
          style="border-color: var(--ds-border); background-color: var(--ds-surface);"
          data-testid="support-queue-bulk-team-select"
          bind:value={bulkTeamId}
        >
          <option value="">{t('supportQueue.bulkAssignTeam')}</option>
          {#each teams as team (team.id)}
            <option value={String(team.id)}>{team.name}</option>
          {/each}
        </select>
        <button
          type="button"
          class="px-2.5 py-1 rounded text-sm border"
          style="border-color: var(--ds-border);"
          data-testid="support-queue-bulk-team-apply"
          disabled={bulkRunning || !bulkTeamId}
          onclick={bulkAssignToTeam}
        >
          {t('supportQueue.bulkApplyTeam')}
        </button>
        <button
          type="button"
          class="px-2.5 py-1 rounded text-sm"
          style="color: var(--ds-text-subtle);"
          data-testid="support-queue-bulk-clear"
          onclick={clearSelection}
        >
          {t('supportQueue.bulkClear')}
        </button>
      </div>
    {/if}

    {#if loadingItems}
      <div class="flex items-center gap-2 py-8">
        <Spinner class="w-4 h-4" />
      </div>
    {:else if items.length === 0}
      <div data-testid="support-queue-empty">
        <EmptyState title={t('supportQueue.empty')} description={t('supportQueue.emptyDescription')} />
      </div>
    {:else}
      <div class="rounded-md border overflow-x-auto" style="border-color: var(--ds-border);">
        <table class="w-full text-sm" data-testid="support-queue-table">
          <thead>
            <tr class="text-left" style="border-bottom: 1px solid var(--ds-border);">
              <th class="px-3 py-2 w-8">
                <input
                  type="checkbox"
                  checked={allSelected}
                  aria-label={t('supportQueue.selectAll')}
                  data-testid="support-queue-select-all"
                  onchange={toggleAll}
                />
              </th>
              <th class="px-3 py-2">{t('supportQueue.columnTitle')}</th>
              <th class="px-3 py-2">{t('supportQueue.columnStatus')}</th>
              <th class="px-3 py-2">{t('supportQueue.columnPriority')}</th>
              <th class="px-3 py-2">{t('supportQueue.columnOwner')}</th>
              <th class="px-3 py-2">{t('supportQueue.columnDue')}</th>
              <th class="px-3 py-2">{t('supportQueue.columnUpdated')}</th>
            </tr>
          </thead>
          <tbody>
            {#each items as item (item.id)}
              <tr
                class="transition-colors"
                style="border-bottom: 1px solid var(--ds-border);"
                data-testid={`support-queue-row-${item.id}`}
              >
                <td class="px-3 py-2">
                  <input
                    type="checkbox"
                    checked={selectedIds.has(item.id)}
                    data-testid={`support-queue-item-checkbox-${item.id}`}
                    onchange={() => toggleItem(item.id)}
                  />
                </td>
                <td class="px-3 py-2">
                  <a
                    class="hover:underline"
                    href={`/workspaces/${workspaceId}/items/${item.id}`}
                    data-testid={`support-queue-item-title-${item.id}`}
                  >
                    {item.workspace_key}-{item.workspace_item_number}
                    {item.title}
                  </a>
                </td>
                <td class="px-3 py-2">
                  <span
                    class="inline-block rounded px-1.5 py-0.5 text-xs"
                    style="background-color: {statusColor(item.status_id)}22; color: {statusColor(item.status_id)};"
                  >
                    {statusName(item.status_id)}
                  </span>
                </td>
                <td class="px-3 py-2">{priorityName(item.priority_id)}</td>
                <td class="px-3 py-2">{ownerLabel(item)}</td>
                <td class="px-3 py-2">{formatDueDate(item.due_date)}</td>
                <td class="px-3 py-2 text-ds-text-subtle">{formatUpdatedAt(item.updated_at)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      {#if itemsTruncated}
        <p class="mt-2 text-xs text-ds-text-subtle">
          {t('supportQueue.showingFirst', { n: items.length })}
        </p>
      {/if}
    {/if}
  {/if}
</div>
