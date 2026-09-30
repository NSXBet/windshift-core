import { api } from '../api.js';
import { workspaceViewItems } from '../navigation/workspaceNavigation.js';

// Every collection-scoped view, used whenever a scope has no explicit
// enabled-views setting or its lookup fails.
const ALL_VIEW_IDS = workspaceViewItems.map((view) => view.id);

/** @typedef {{ views: string[], inherited: boolean, loaded: boolean }} ViewSettingsEntry */

/** @type {ViewSettingsEntry} */
const DEFAULT_ENTRY = { views: ALL_VIEW_IDS, inherited: true, loaded: false };

function createViewSettingsStore() {
  /** @type {Record<string, ViewSettingsEntry>} */
  let entries = $state({});
  const inflight = new Map();

  function scopeKey(workspaceId, collectionId) {
    return `${collectionId ?? ''}|${workspaceId ?? ''}`;
  }

  /**
   * Loads the effective enabled views for a scope. Missing or invalid
   * settings resolve to every view enabled; failures are cached as the
   * default so navigation never blocks on the lookup. Writes to the store
   * happen exactly once per load so effects reading entries settle.
   */
  async function load(workspaceId, collectionId = null) {
    const key = scopeKey(workspaceId, collectionId);
    if (entries[key]?.loaded) return entries[key];

    const pending = inflight.get(key);
    if (pending) return pending;

    const promise = (async () => {
      let entry = DEFAULT_ENTRY;
      try {
        const config = await api.collections.getBoardConfiguration(collectionId, workspaceId);
        const enabled = config?.view_settings?.enabled_views;
        entry = {
          views: Array.isArray(enabled) && enabled.length > 0 ? enabled : ALL_VIEW_IDS,
          inherited: Boolean(config?.view_settings_inherited),
          loaded: true,
        };
      } catch {
        // Keep the default entry.
      }
      entries[key] = entry;
      return entry;
    })();
    inflight.set(key, promise);
    try {
      return await promise;
    } finally {
      inflight.delete(key);
    }
  }

  /** Enabled view ids for a scope; every view while unloaded. */
  function enabledViewIds(workspaceId, collectionId = null) {
    return entries[scopeKey(workspaceId, collectionId)]?.views ?? ALL_VIEW_IDS;
  }

  /** The raw scope entry (reactive), or null before the first load. */
  function entryFor(workspaceId, collectionId = null) {
    return entries[scopeKey(workspaceId, collectionId)] ?? null;
  }

  /** Whether the scope's settings come from the workspace default. */
  function inherited(workspaceId, collectionId = null) {
    return entries[scopeKey(workspaceId, collectionId)]?.inherited ?? true;
  }

  function invalidate(workspaceId, collectionId = null) {
    delete entries[scopeKey(workspaceId, collectionId)];
  }

  /** Drops the workspace scope and every collection cache under it. */
  function invalidateWorkspace(workspaceId) {
    const prefix = `${workspaceId ?? ''}`;
    for (const key of Object.keys(entries)) {
      if (key.split('|')[1] === prefix) delete entries[key];
    }
  }

  return {
    load,
    entryFor,
    enabledViewIds,
    inherited,
    invalidate,
    invalidateWorkspace,
    allViewIds: ALL_VIEW_IDS,
  };
}

export const viewSettingsStore = createViewSettingsStore();
