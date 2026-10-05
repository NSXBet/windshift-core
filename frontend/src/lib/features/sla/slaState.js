import { api } from '../../api.js';

// Item SLA reads are pure and cheap on the server, but a list/board page shows
// many rows. Cache each item's state briefly and dedupe concurrent reads so a
// page issues at most one request per item, and none at all on a re-render.
const TTL_MS = 30_000;
const stateCache = new Map();
const inFlight = new Map();
const thresholdsCache = new Map();
let cacheGeneration = 0;

// Batch coalescing: badges mounted in the same tick share one workspace
// batch read instead of issuing one request per row (WI-1591). Items whose
// workspace is unknown fall back to per-item reads; a failed batch does not.
const pendingByWorkspace = new Map();

// Server cap for one batch read (WI-1637). Boards can mount more cards than
// this, so ids are split across requests instead of being rejected whole.
const BATCH_ID_LIMIT = 200;

export async function getItemSLA(itemId, workspaceId = null) {
  if (!itemId) return [];
  const cached = stateCache.get(itemId);
  const now = Date.now();
  if (cached && now - cached.at < TTL_MS) return cached.value;
  if (inFlight.has(itemId)) return inFlight.get(itemId);

  if (workspaceId && api.sla?.getItemSLABatch) {
    return enqueueBatchItemSLA(itemId, workspaceId);
  }
  if (!api.sla?.getItemSLA) return [];
  return fetchItemSLA(itemId);
}

// enqueueBatchItemSLA collects item ids per workspace and flushes them on the
// next microtask, so all badges rendered by one page share batched requests.
function enqueueBatchItemSLA(itemId, workspaceId) {
  let queue = pendingByWorkspace.get(workspaceId);
  if (!queue) {
    queue = { ids: new Set(), waiters: new Map(), scheduled: false };
    pendingByWorkspace.set(workspaceId, queue);
  }
  if (queue.waiters.has(itemId)) return queue.waiters.get(itemId);
  queue.ids.add(itemId);
  const promise = new Promise((resolve) => queue.waiters.set(itemId, resolve));
  if (!queue.scheduled) {
    queue.scheduled = true;
    queueMicrotask(() => flushBatchItemSLA(workspaceId, queue));
  }
  return promise;
}

async function flushBatchItemSLA(workspaceId, queue) {
  pendingByWorkspace.delete(workspaceId);
  const ids = [...queue.ids];
  const generation = cacheGeneration;
  const results = new Map();
  const chunks = [];
  for (let i = 0; i < ids.length; i += BATCH_ID_LIMIT) {
    chunks.push(ids.slice(i, i + BATCH_ID_LIMIT));
  }
  await Promise.all(
    chunks.map(async (chunkIds) => {
      let batch;
      try {
        batch = (await api.sla.getItemSLABatch(workspaceId, chunkIds)) ?? {};
      } catch {
        // A failed chunk resolves unset rather than issuing one request per
        // item, which would amplify load under rate limiting or outages.
        return;
      }
      for (const itemId of chunkIds) {
        results.set(itemId, batch[itemId] ?? batch[String(itemId)] ?? []);
      }
    })
  );
  for (const itemId of ids) {
    const resolve = queue.waiters.get(itemId);
    if (!resolve) continue;
    if (!results.has(itemId)) {
      // Leave the cache untouched so the next mount retries the batch.
      resolve([]);
      continue;
    }
    const value = results.get(itemId);
    if (generation === cacheGeneration) {
      stateCache.set(itemId, { value, at: Date.now() });
    }
    resolve(value);
  }
}

// fetchItemSLA issues the per-item read and caches the result.
function fetchItemSLA(itemId) {
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
  // Waiters of an in-flight batch still resolve with the values they fetched.
  pendingByWorkspace.clear();
}

if (typeof window !== 'undefined') {
  window.addEventListener('refresh-work-items', invalidateSLAState);
}
