<script>
  import { onDestroy, onMount } from 'svelte';
  import { useEventListener } from 'runed';
  import { Copy } from '@lucide/svelte';
  import Spinner from '../../components/Spinner.svelte';
  import Lozenge from '../../components/Lozenge.svelte';
  import Text from '../../components/Text.svelte';
  import DescriptionText from '../../components/DescriptionText.svelte';
  import ItemKey from '../../components/ItemKey.svelte';
  import Button from '../../components/Button.svelte';
  import { t } from '../../stores/i18n.svelte.js';
  import { api } from '../../api.js';

  let { itemId, canEdit = false, onrequestmerge = null } = $props();

  let loading = $state(true);
  let tickets = $state([]);
  let loadController = null;

  async function load() {
    loadController?.abort();
    loadController = new AbortController();
    loading = true;
    try {
      const response = await api.items.requesterOpenTickets(itemId, {
        signal: loadController.signal
      });
      tickets = response ?? [];
    } catch {
      tickets = [];
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    load();
  });

  onDestroy(() => loadController?.abort());

  // Merging a candidate removes it from this list; refresh without waiting
  // for a full detail reload.
  useEventListener(() => window, 'reload-item-detail', (/** @type {CustomEvent<{itemId?: number|string}>} */ event) => {
    const id = event?.detail?.itemId;
    if (id == null || String(id) !== String(itemId)) return;
    load();
  });
</script>

{#if loading}
  <div class="pt-4 mt-4 border-t flex justify-center" style="border-color: var(--ds-border);">
    <Spinner size="small" />
  </div>
{:else if tickets.length > 0}
  <div class="pt-4 mt-4 border-t" style="border-color: var(--ds-border);">
    <div class="flex items-center gap-2">
      <Copy class="w-3.5 h-3.5 flex-shrink-0" style="color: var(--ds-text-subtle);" />
      <Text variant="subtle" size="xs" weight="semibold" class="uppercase tracking-wider">
        {t('items.requesterOpenTickets')}
      </Text>
    </div>
    <div class="mt-3 space-y-2" data-testid="requester-open-tickets">
      <DescriptionText>
        {t('items.requesterOpenTicketsHelp')}
      </DescriptionText>
      {#each tickets as ticket (ticket.id)}
        <div
          class="p-2.5 rounded border transition-colors"
          style="border-color: var(--ds-border); background: var(--ds-surface-raised);"
        >
          <div class="flex items-start justify-between gap-2">
            <a
              href={`/workspaces/${ticket.workspace_id}/items/${ticket.id}`}
              data-testid={`requester-open-ticket-${ticket.id}`}
              class="flex-1 min-w-0 block hover:opacity-90"
            >
              <div class="flex items-center justify-between gap-2">
                <ItemKey item={ticket} />
                {#if ticket.status_name}
                  <Lozenge color="gray">{ticket.status_name}</Lozenge>
                {/if}
              </div>
              <div class="mt-1 text-sm truncate" style="color: var(--ds-text);">
                {ticket.title}
              </div>
            </a>
            {#if canEdit && onrequestmerge}
              <Button
                variant="secondary"
                size="small"
                dataTestid={`requester-open-ticket-merge-${ticket.id}`}
                onclick={() => onrequestmerge(ticket)}
              >
                {t('items.mergeConfirm')}
              </Button>
            {/if}
          </div>
        </div>
      {/each}
    </div>
  </div>
{/if}
