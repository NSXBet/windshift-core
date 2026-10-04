<script>
  import { onMount } from 'svelte';
  import { Users, X, Plus } from '@lucide/svelte';
  import Spinner from '../../components/Spinner.svelte';
  import Text from '../../components/Text.svelte';
  import PortalCustomerPicker from '../../pickers/PortalCustomerPicker.svelte';
  import { t } from '../../stores/i18n.svelte.js';
  import { errorToast } from '../../stores/toasts.svelte.js';
  import { api } from '../../api.js';

  let { itemId, canEdit = true } = $props();

  let participants = $state([]);
  let loading = $state(true);
  let email = $state('');
  let name = $state('');
  let adding = $state(false);

  async function load() {
    loading = true;
    try {
      participants = (await api.items.listParticipants(itemId)) ?? [];
    } catch {
      participants = [];
    } finally {
      loading = false;
    }
  }

  onMount(load);

  async function addByEmail() {
    const value = email.trim();
    if (!value || adding) return;
    adding = true;
    try {
      participants =
        (await api.items.addParticipant(itemId, {
          email: value,
          name: name.trim() || undefined
        })) ?? [];
      email = '';
      name = '';
    } catch (error) {
      errorToast(error?.message || t('items.participantsAddFailed'));
    } finally {
      adding = false;
    }
  }

  async function addByCustomer(customer) {
    if (!customer?.id) return;
    try {
      participants = (await api.items.addParticipant(itemId, { portal_customer_id: customer.id })) ?? [];
    } catch (error) {
      errorToast(error?.message || t('items.participantsAddFailed'));
    }
  }

  async function remove(participant) {
    try {
      await api.items.removeParticipant(itemId, participant.portal_customer_id);
      participants = participants.filter((p) => p.portal_customer_id !== participant.portal_customer_id);
    } catch (error) {
      errorToast(error?.message || t('items.participantsRemoveFailed'));
    }
  }
</script>

<div class="pt-4 mt-4 border-t" style="border-color: var(--ds-border);">
  <div class="flex items-center gap-2 text-sm font-semibold" style="color: var(--ds-text);">
    <Users class="w-4 h-4" style="color: var(--ds-text-subtle);" />
    {t('items.participants')}
  </div>

  {#if loading}
    <div class="pt-3 flex justify-center">
      <Spinner size="small" />
    </div>
  {:else}
    <div class="mt-3 space-y-2" data-testid="item-participants">
      {#if participants.length === 0}
        <div data-testid="item-participants-empty">
          <Text variant="subtle" size="sm">{t('items.participantsEmpty')}</Text>
        </div>
      {/if}
      {#each participants as participant (participant.portal_customer_id)}
        <div
          class="flex items-center justify-between gap-2 px-2 py-1.5 rounded border"
          style="border-color: var(--ds-border); background: var(--ds-surface-raised);"
          data-testid={`item-participant-${participant.portal_customer_id}`}
        >
          <div class="min-w-0">
            <div class="text-sm truncate" style="color: var(--ds-text);">
              {participant.customer_name || participant.customer_email}
            </div>
            {#if participant.customer_name && participant.customer_email}
              <div class="text-xs truncate" style="color: var(--ds-text-subtle);">
                {participant.customer_email}
              </div>
            {/if}
          </div>
          {#if canEdit}
            <button
              type="button"
              class="p-1 rounded hover-bg flex-shrink-0"
              title={t('items.participantRemove')}
              data-testid={`item-participant-remove-${participant.portal_customer_id}`}
              onclick={() => remove(participant)}
            >
              <X class="w-3.5 h-3.5" style="color: var(--ds-text-subtle);" />
            </button>
          {/if}
        </div>
      {/each}
    </div>

    {#if canEdit}
      <div class="mt-3 space-y-2">
        <input
          type="email"
          bind:value={email}
          placeholder={t('items.participantEmailPlaceholder')}
          data-testid="item-participant-email"
          class="w-full px-2 py-1.5 text-sm rounded border"
          style="border-color: var(--ds-border); background: var(--ds-background-input); color: var(--ds-text);"
          onkeydown={(event) => {
            if (event.key === 'Enter') addByEmail();
          }}
        />
        <input
          type="text"
          bind:value={name}
          placeholder={t('items.participantNamePlaceholder')}
          data-testid="item-participant-name"
          class="w-full px-2 py-1.5 text-sm rounded border"
          style="border-color: var(--ds-border); background: var(--ds-background-input); color: var(--ds-text);"
        />
        <button
          type="button"
          class="w-full flex items-center justify-center gap-1 px-2 py-1.5 text-sm rounded border hover-bg disabled:opacity-50"
          style="border-color: var(--ds-border); color: var(--ds-text);"
          disabled={adding || !email.trim()}
          data-testid="item-participant-add"
          onclick={addByEmail}
        >
          <Plus class="w-3.5 h-3.5" />
          {t('items.participantAdd')}
        </button>
        <PortalCustomerPicker
          value={null}
          placeholder={t('items.participantPickExisting')}
          class="w-full"
          onSelect={addByCustomer}
        />
      </div>
    {/if}
  {/if}
</div>
