<script>
  import { onDestroy, onMount } from 'svelte';
  import { Users } from '@lucide/svelte';
  import Spinner from '../../components/Spinner.svelte';
  import Lozenge from '../../components/Lozenge.svelte';
  import Text from '../../components/Text.svelte';
  import DescriptionText from '../../components/DescriptionText.svelte';
  import ItemKey from '../../components/ItemKey.svelte';
  import { t } from '../../stores/i18n.svelte.js';
  import { api } from '../../api.js';

  let { itemId } = $props();

  let loading = $state(true);
  let tickets = $state([]);

  const loadController = new AbortController();
  onDestroy(() => loadController.abort());

  onMount(async () => {
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
  });
</script>

{#if loading}
  <div class="pt-4 mt-4 border-t flex justify-center" style="border-color: var(--ds-border);">
    <Spinner size="small" />
  </div>
{:else if tickets.length > 0}
  <div class="pt-4 mt-4 border-t" style="border-color: var(--ds-border);">
    <div class="flex items-center gap-2">
      <Users class="w-3.5 h-3.5 flex-shrink-0" style="color: var(--ds-text-subtle);" />
      <Text variant="subtle" size="xs" weight="semibold" class="uppercase tracking-wider">
        {t('items.requesterOpenTickets')}
      </Text>
    </div>
    <div class="mt-3 space-y-2" data-testid="requester-open-tickets">
      <DescriptionText>
        {t('items.requesterOpenTicketsHelp')}
      </DescriptionText>
      {#each tickets as ticket (ticket.id)}
        <a
          href={`/workspaces/${ticket.workspace_id}/items/${ticket.id}`}
          data-testid={`requester-open-ticket-${ticket.id}`}
          class="block p-2.5 rounded border transition-colors hover:opacity-90"
          style="border-color: var(--ds-border); background: var(--ds-surface-raised);"
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
      {/each}
    </div>
  </div>
{/if}
