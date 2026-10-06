<script>
  import { onMount } from 'svelte';
  import { IconPlus, IconTrash, IconPencil } from '@tabler/icons-svelte-runes';
  import { api } from '../../api.js';
  import { t } from '../../stores/i18n.svelte.js';
  import Button from '../../components/Button.svelte';
  import TextField from '../../components/TextField.svelte';
  import SelectField from '../../components/SelectField.svelte';
  import Label from '../../components/Label.svelte';
  import Lozenge from '../../components/Lozenge.svelte';
  import DescriptionText from '../../components/DescriptionText.svelte';
  import StateDisplay from '../../components/StateDisplay.svelte';
  import WorkspaceSelector from '../../workspaces/WorkspaceSelector.svelte';

  let {
    channelId,
    workspaces = [],
    itemTypes = [],
    portals = [],
    onToast = () => {},
    onLoadItemTypes = () => {}
  } = $props();

  let intakes = $state([]);
  let loading = $state(false);
  let editing = $state(null);

  function blankIntake() {
    return {
      id: 0,
      folder: 'INBOX',
      target_type: 'workspace',
      target_id: null,
      item_type_id: null,
      rate_limit_per_hour: null,
      processing_disposition: '',
      status: 'enabled'
    };
  }

  async function load() {
    if (!channelId) return;
    loading = true;
    try {
      intakes = (await api.channelIntakes.list(channelId)) ?? [];
    } catch (err) {
      console.error('Failed to load intakes:', err);
      onToast(t('channel.intakeLoadFailed'));
    } finally {
      loading = false;
    }
  }

  onMount(load);

  function startCreate() {
    editing = blankIntake();
  }

  function startEdit(intake) {
    editing = { ...intake };
  }

  async function save() {
    if (!editing.folder?.trim()) {
      onToast(t('channel.intakeFolderRequired'));
      return;
    }
    if (!editing.target_id) {
      onToast(t('channel.intakeTargetRequired'));
      return;
    }
    if (editing.target_type === 'workspace' && !editing.item_type_id) {
      onToast(t('channel.itemTypeRequired'));
      return;
    }
    const payload = {
      folder: editing.folder.trim(),
      target_type: editing.target_type,
      target_id: Number(editing.target_id),
      item_type_id: editing.target_type === 'workspace' ? editing.item_type_id : null,
      rate_limit_per_hour:
        editing.rate_limit_per_hour === '' || editing.rate_limit_per_hour === null
          ? null
          : Number(editing.rate_limit_per_hour),
      processing_disposition: editing.processing_disposition || '',
      status: editing.status || 'enabled'
    };
    try {
      if (editing.id) {
        await api.channelIntakes.update(channelId, editing.id, payload);
      } else {
        await api.channelIntakes.create(channelId, payload);
      }
      editing = null;
      await load();
      onToast(t('channel.intakeSaved'));
    } catch (err) {
      onToast(err?.message || t('channel.intakeSaveFailed'));
    }
  }

  async function remove(intake) {
    try {
      await api.channelIntakes.delete(channelId, intake.id);
      await load();
      onToast(t('channel.intakeDeleted'));
    } catch (err) {
      onToast(err?.message || t('channel.intakeDeleteFailed'));
    }
  }

  function targetLabel(intake) {
    if (intake.target_type === 'portal') {
      const portal = portals.find((p) => p.id === intake.target_id);
      return portal ? portal.name : `Portal #${intake.target_id}`;
    }
    const workspace = workspaces.find((w) => w.id === intake.target_id);
    return workspace ? workspace.name : `Workspace #${intake.target_id}`;
  }

  const targetOptions = [
    { value: 'workspace', label: t('channel.intakeTargetWorkspace') },
    { value: 'portal', label: t('channel.intakeTargetPortal') }
  ];

  const dispositionOptions = [
    { value: '', label: t('channel.dispositionInherit') },
    { value: 'leave', label: t('channel.dispositionLeave') },
    { value: 'mark_read', label: t('channel.dispositionMarkRead') },
    { value: 'delete', label: t('channel.dispositionDelete') }
  ];

  const statusOptions = [
    { value: 'enabled', label: t('channel.intakeStatusEnabled') },
    { value: 'disabled', label: t('channel.intakeStatusDisabled') }
  ];
</script>

<div class="pt-6 border-t space-y-4" style="border-color: var(--ds-border);">
  <div>
    <h4 class="text-sm font-semibold" style="color: var(--ds-text);">{t('channel.intakesTitle')}</h4>
    <DescriptionText>{t('channel.intakesHelp')}</DescriptionText>
  </div>

  {#if loading}
    <StateDisplay type="loading" />
  {:else}
    <div class="space-y-2" data-testid="channel-intake-list">
      {#each intakes as intake (intake.id)}
        <div
          class="p-3 rounded border flex items-start justify-between gap-3"
          style="background: var(--ds-surface-raised); border-color: var(--ds-border);"
          data-testid="channel-intake-{intake.id}"
        >
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <span class="font-medium text-sm" style="color: var(--ds-text);">{intake.folder}</span>
              <Lozenge color={intake.status === 'enabled' ? 'green' : 'gray'}>
                {intake.status === 'enabled'
                  ? t('channel.intakeStatusEnabled')
                  : t('channel.intakeStatusDisabled')}
              </Lozenge>
            </div>
            <div class="text-xs mt-1" style="color: var(--ds-text-subtle);">
              {intake.target_type === 'portal'
                ? t('channel.intakeFeedsPortal')
                : t('channel.intakeFeedsWorkspace')}: {targetLabel(intake)}
            </div>
          </div>
          <div class="flex items-center gap-1 flex-shrink-0">
            <Button variant="ghost" size="small" onclick={() => startEdit(intake)}>
              <IconPencil size={14} />
            </Button>
            <Button variant="ghost" size="small" onclick={() => remove(intake)}>
              <IconTrash size={14} />
            </Button>
          </div>
        </div>
      {/each}

      {#if intakes.length === 0}
        <DescriptionText>{t('channel.intakesEmpty')}</DescriptionText>
      {/if}
    </div>

    {#if editing}
      <div class="p-4 rounded border space-y-4" style="background: var(--ds-surface); border-color: var(--ds-border);">
        <div class="grid grid-cols-2 gap-4">
          <TextField
            label={t('channel.intakeFolder')}
            labelColor="default"
            placeholder="INBOX"
            id="intake-folder"
            dataTestid="channel-intake-folder"
            bind:value={editing.folder}
          />
          <SelectField
            label={t('channel.intakeTargetType')}
            labelColor="default"
            id="intake-target-type"
            options={targetOptions}
            bind:value={editing.target_type}
          />
        </div>

        <div class="grid grid-cols-2 gap-4">
          {#if editing.target_type === 'workspace'}
            <div>
              <Label color="default" class="mb-2">{t('channel.intakeTargetWorkspace')}</Label>
              <WorkspaceSelector
                bind:value={editing.target_id}
                {workspaces}
                placeholder={t('channel.selectWorkspace')}
                onSelect={() => {
                  editing.item_type_id = null;
                  onLoadItemTypes(editing.target_id);
                }}
              />
            </div>
            <SelectField
              label={t('channel.itemType')}
              labelColor="default"
              disabled={!editing.target_id}
              options={[
                { value: null, label: t('channel.selectItemType') },
                ...itemTypes.map((type) => ({ value: type.id, label: type.name }))
              ]}
              bind:value={editing.item_type_id}
            />
          {:else}
            <SelectField
              label={t('channel.intakeTargetPortal')}
              labelColor="default"
              id="intake-target-portal"
              options={[
                { value: null, label: t('channel.selectPortal') },
                ...portals.map((portal) => ({ value: portal.id, label: portal.name }))
              ]}
              bind:value={editing.target_id}
            />
            <div>
              <DescriptionText>{t('channel.intakePortalRequestTypeHelp')}</DescriptionText>
            </div>
          {/if}
        </div>

        <div class="grid grid-cols-3 gap-4">
          <TextField
            label={t('channel.rateLimitPerHour')}
            labelColor="default"
            type="number"
            min="0"
            placeholder="100"
            bind:value={editing.rate_limit_per_hour}
          />
          <SelectField
            label={t('channel.processingDisposition')}
            labelColor="default"
            id="intake-disposition"
            options={dispositionOptions}
            bind:value={editing.processing_disposition}
          />
          <SelectField
            label={t('channel.intakeStatus')}
            labelColor="default"
            id="intake-status"
            options={statusOptions}
            bind:value={editing.status}
          />
        </div>

        <div class="flex justify-end gap-2">
          <Button variant="ghost" onclick={() => (editing = null)}>{t('common.cancel')}</Button>
          <Button variant="primary" dataTestid="channel-intake-save" onclick={save}>{t('common.save')}</Button>
        </div>
      </div>
    {:else}
      <button
        type="button"
        onclick={startCreate}
        class="w-full flex items-center justify-center gap-2 px-4 py-3 rounded border-2 border-dashed transition-all"
        style="border-color: var(--ds-border); color: var(--ds-text-subtle);"
        data-testid="channel-intake-add"
      >
        <IconPlus size={16} />
        <span class="font-medium">{t('channel.intakeAdd')}</span>
      </button>
    {/if}
  {/if}
</div>
