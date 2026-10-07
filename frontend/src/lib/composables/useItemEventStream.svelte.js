import { toExternal } from '../runtime/contextPath.js';
import { itemLiveUpdates } from '../stores/itemLiveUpdates.svelte.js';

const DEBOUNCE_MS = 250;

/**
 * Subscribes to an item's event stream and batches targeted reloads. Every
 * healthy connection reconciles, because the initial snapshot can be served
 * before the stream is actually live. Polling remains the fallback while
 * disconnected or after a refresh failure.
 *
 * @param {() => (number|string|null|undefined)} getItemId
 * @param {{ onReconcile?: Function, onItem?: Function, onChildren?: Function, onComment?: Function, onLinks?: Function, onZammad?: Function, onDeleted?: Function }} handlers
 * @returns {{ readonly connected: boolean }}
 */
export function useItemEventStream(getItemId, handlers = {}) {
  let connected = $state(false);
  let streamItemId = $derived(normalizeItemEventStreamID(getItemId()));

  $effect(() => {
    const itemId = streamItemId;
    if (!itemId) return;
    if (typeof EventSource === 'undefined') return; // SSR / unsupported → polling stays the source of truth

    const pending = new Set();
    let timer = null;
    // A handler that was already running when the subscription was torn down
    // must not reconcile against the next item's view.
    let active = true;
    const connectionTracker = createConnectionReconcileTracker();

    const flush = async () => {
      timer = null;
      const kinds = new Set(pending);
      pending.clear();
      if (!active) return;
      try {
        // A full reconcile reloads everything, so skip the narrower reloads.
        if (kinds.has('reconcile')) {
          await handlers.onReconcile?.();
          return;
        }
        const refreshes = [];
        if (kinds.has('item')) refreshes.push(handlers.onItem?.());
        if (kinds.has('children')) refreshes.push(handlers.onChildren?.());
        if (kinds.has('comment')) refreshes.push(handlers.onComment?.());
        if (kinds.has('links')) refreshes.push(handlers.onLinks?.());
        if (kinds.has('zammad')) refreshes.push(handlers.onZammad?.());
        if (kinds.has('deleted')) refreshes.push(handlers.onDeleted?.());
        await Promise.all(refreshes);
      } catch (error) {
        if (!active) return;
        // The transport is healthy but a data refresh failed, so the view may
        // be stale. Resume the polling fallback and reconcile on next connect.
        connectionTracker.markDisconnected();
        setConnected(false);
        console.warn('Item event stream refresh failed; falling back to polling:', error);
      }
    };
    const schedule = (...kinds) => {
      for (const k of kinds) pending.add(k);
      if (!timer) timer = setTimeout(flush, DEBOUNCE_MS);
    };

    const es = new EventSource(toExternal(`/api/items/${itemId}/events`));
    const setConnected = (value) => {
      connected = value;
      itemLiveUpdates.set(itemId, value);
    };

    es.addEventListener('connected', () => {
      const shouldReconcile = connectionTracker.markConnected();
      setConnected(true);
      if (shouldReconcile) schedule('reconcile');
    });
    es.addEventListener('reload', () => schedule('reconcile'));
    es.addEventListener('comment', () => schedule('comment'));
    es.addEventListener('status', () => schedule('item'));
    es.addEventListener('updated', () => schedule('item', 'children'));
    es.addEventListener('created', () => schedule('item', 'children'));
    es.addEventListener('deleted', () => {
      // Deletion is authoritative and must tear down item-bound loaders before
      // their in-flight requests start returning expected 404s.
      if (timer) clearTimeout(timer);
      timer = null;
      pending.clear();
      handlers.onDeleted?.();
    });
    es.addEventListener('link', () => schedule('links'));
    es.addEventListener('zammad', () => schedule('zammad'));
    // The browser auto-reconnects (honoring the server's retry hint). Until it
    // does, mark disconnected so the components' pollers resume as the fallback.
    es.onerror = () => {
      connectionTracker.markDisconnected();
      setConnected(false);
    };

    return () => {
      active = false;
      if (timer) clearTimeout(timer);
      es.close();
      itemLiveUpdates.clear(itemId);
      connected = false;
    };
  });

  return {
    get connected() {
      return connected;
    },
  };
}

// Route parameters begin as strings and are later hydrated from API records as
// numbers. Keep that representation-only change from reopening the stream.
export function normalizeItemEventStreamID(itemId) {
  return itemId ? String(itemId) : null;
}

/**
 * Track whether a `connected` event needs a reconcile. A healthy connection
 * always does: constructing the EventSource only starts the subscription, so
 * the caller's initial snapshot can be served before the stream is live and a
 * change in that window is never replayed. Exported as a pure helper for
 * regression tests.
 */
export function createConnectionReconcileTracker() {
  return {
    markConnected() {
      return true;
    },
    markDisconnected() {},
  };
}
