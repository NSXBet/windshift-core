import { api } from '../../api.js';

// Item SLA reads are pure and cheap on the server, but a list/board page shows
// many rows. Cache each item's state briefly and dedupe concurrent reads so a
// page issues at most one request per item, and none at all on a re-render.
const TTL_MS = 30_000;
const stateCache = new Map();
const inFlight = new Map();
const thresholdsCache = new Map();
let cacheGeneration = 0;

export async function getItemSLA(itemId) {
  if (!itemId || !api.sla?.getItemSLA) return [];
  const cached = stateCache.get(itemId);
  const now = Date.now();
  if (cached && now - cached.at < TTL_MS) return cached.value;
  if (inFlight.has(itemId)) return inFlight.get(itemId);

  const generation = cacheGeneration;
  let request;
  request = api.sla
    .getItemSLA(itemId)
    .then((value) => {
      if (generation === cacheGeneration) {
        stateCache.set(itemId, { value: value ?? [], at: Date.now() });
      }
      return value ?? [];
    })
    .catch(() => [])
    .finally(() => {
      if (inFlight.get(itemId) === request) inFlight.delete(itemId);
    });

  inFlight.set(itemId, request);
  return request;
}

export async function getSLAThresholds(workspaceId) {
  if (!workspaceId || !api.sla?.getWarningThresholds) return [];
  const cached = thresholdsCache.get(workspaceId);
  if (cached && Date.now() - cached.at < TTL_MS) return cached.value;
  const generation = cacheGeneration;
  const value = (await api.sla.getWarningThresholds(workspaceId).catch(() => [])) ?? [];
  if (generation === cacheGeneration) thresholdsCache.set(workspaceId, { value, at: Date.now() });
  return value;
}

// Drop cached state when an item changes so the next render re-derives it.
export function invalidateSLAState() {
  cacheGeneration++;
  inFlight.clear();
  stateCache.clear();
  thresholdsCache.clear();
}

if (typeof window !== 'undefined') {
  window.addEventListener('refresh-work-items', invalidateSLAState);
}
