import { viewSettingsStore } from '../stores/viewSettings.svelte.js';

// The collection-scoped views this guard protects, keyed by the
// workspace-<view> route suffix.
const GUARDED_VIEW_IDS = new Set(viewSettingsStore.allViewIds);

// Workspace-scope nav entries this guard protects. Their route views are
// workspace-<id> for tools and the bare test ids for test management; both
// map back to the nav id and redirect to the workspace default view when
// the workspace nav settings disable them.
const GUARDED_NAV_VIEW_IDS = new Set(
  viewSettingsStore.allNavIds.filter((id) => !GUARDED_VIEW_IDS.has(id))
);
const TEST_ROUTE_VIEW_TO_NAV_ID = new Map([
  ['test-cases', 'test-cases'],
  ['test-sets', 'test-sets'],
  ['test-templates', 'test-templates'],
  ['test-runs', 'test-runs'],
  ['test-reports', 'test-reports'],
]);

/**
 * Resolves the redirect for a route pointing at a view the current scope has
 * disabled, or null when the route may stay. The workspace default view wins
 * when it is still enabled; otherwise the first enabled view takes over.
 */
export function getDisabledViewRedirect(route, workspace, viewSettings = viewSettingsStore) {
  const view = route?.view;
  if (!view) return null;
  const workspaceId = route?.params?.id;
  if (!workspaceId) return null;

  // Workspace-scope nav entries: disabled entries bounce to the workspace
  // default view; the test routes use their own view names.
  const navId = view.startsWith('workspace-')
    ? view.slice('workspace-'.length)
    : TEST_ROUTE_VIEW_TO_NAV_ID.get(view);
  if (navId && GUARDED_NAV_VIEW_IDS.has(navId)) {
    const enabled = new Set(viewSettings.enabledNavIds(workspaceId));
    if (!enabled.has(navId)) return `/workspaces/${workspaceId}`;
    return null;
  }

  if (!view.startsWith('workspace-')) return null;
  const viewId = view.slice('workspace-'.length);
  if (!GUARDED_VIEW_IDS.has(viewId)) return null;

  const collectionId = route?.params?.collectionId ?? null;

  const enabled = viewSettings.enabledViewIds(workspaceId, collectionId);
  if (enabled.includes(viewId)) return null;

  const workspaceDefault = workspace?.default_view;
  const target =
    workspaceDefault && enabled.includes(workspaceDefault) ? workspaceDefault : enabled[0];
  if (!target) return null;

  const prefix = collectionId ? `/collections/${collectionId}` : '';
  return `/workspaces/${workspaceId}${prefix}/${target}`;
}
