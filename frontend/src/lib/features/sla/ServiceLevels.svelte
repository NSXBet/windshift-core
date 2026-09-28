<script>
  import SlaCalendarsTab from './SlaCalendarsTab.svelte';
  import { Calendar } from '@lucide/svelte';
  import { t } from '../../stores/i18n.svelte.js';

  let { workspaceId = null } = $props();

  // Sub-sections of the Service levels area. Each entry maps to a component
  // that reads the active workspace from the prop.
  const tabs = [
    {
      id: 'calendars',
      labelKey: 'workspaceSettings.serviceLevels.tabs.calendars',
      icon: Calendar,
      component: SlaCalendarsTab,
    },
  ];

  let active = $state(tabs[0].id);
</script>

<div class="space-y-4" data-testid="service-levels">
  {#if tabs.length > 1}
    <div class="flex gap-1 border-b" style="border-color: var(--ds-border)" role="tablist">
      {#each tabs as tab (tab.id)}
        {@const TabIcon = tab.icon}
        <button
          type="button"
          role="tab"
          aria-selected={active === tab.id}
          class="px-3 py-2 text-sm font-medium flex items-center gap-2"
          class:active={active === tab.id}
          data-testid="service-levels-tab-{tab.id}"
          onclick={() => (active = tab.id)}
        >
          <TabIcon class="w-4 h-4" />
          {t(tab.labelKey)}
        </button>
      {/each}
    </div>
  {/if}

  {#each tabs as tab (tab.id)}
    {#if active === tab.id}
      {@const TabComponent = tab.component}
      <TabComponent {workspaceId} />
    {/if}
  {/each}
</div>
