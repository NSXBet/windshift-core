<script>
  import LinkComponent from './Link.svelte';
  import { formatItemKey } from '../utils/itemKey.js';

  /**
   * Inline work-item key. `plain` renders the mono key on its own; `badge`
   * renders the neutral metadata chip used in ticket lists and card rows.
   * Accepts an `item` object or a pre-formatted `itemKey` string.
   */
  let {
    item = null,
    itemKey = null,
    workspace = null,
    size = 'default',
    variant = 'plain',
    style = null,
    href = null,
    onClick = null,
    monospace = true,
    dataTestid = undefined,
    title = undefined,
    class: className = '',
  } = $props();

  const displayKey = $derived.by(() => {
    if (itemKey) return itemKey;
    if (!item) return '';
    if (item.item_key) return item.item_key;
    const fromFields = formatItemKey(item);
    if (fromFields) return fromFields;
    const key = item.workspace_key || workspace?.key;
    return key ? `${key}-${item.workspace_item_number}` : `ITEM-${item.workspace_item_number}`;
  });

  const interactive = $derived(!!(href || onClick));
  const badge = $derived(variant === 'badge');

  const sizeClass = $derived.by(() => {
    if (badge) {
      return size === 'compact'
        ? 'text-[10px] leading-4 px-1 py-0.5'
        : 'text-xs leading-4 px-1.5 py-0.5';
    }
    return size === 'compact' ? 'text-[10px] leading-4 tracking-[0.02em]' : 'text-xs';
  });

  const classes = $derived.by(() => {
    if (badge) {
      return [
        'inline-flex items-center rounded flex-shrink-0 whitespace-nowrap',
        monospace ? 'font-mono' : '',
        sizeClass,
        interactive ? 'transition-colors hover:bg-[var(--ds-background-neutral-hovered)] cursor-pointer' : '',
        className,
      ].filter(Boolean).join(' ');
    }
    return [
      sizeClass,
      monospace ? 'font-mono' : '',
      'flex-shrink-0 whitespace-nowrap',
      interactive ? 'hover:underline cursor-pointer' : '',
      className,
    ].filter(Boolean).join(' ');
  });

  const resolvedStyle = $derived(
    style ??
      (badge
        ? 'background-color: var(--ds-background-neutral); color: var(--ds-text-subtle);'
        : 'color: var(--ds-text-subtle);'),
  );
</script>

{#if href}
  <LinkComponent {href} {onClick} class={classes} style={resolvedStyle} data-testid={dataTestid} {title}>
    {displayKey}
  </LinkComponent>
{:else if onClick}
  <button class={classes} style={resolvedStyle} onclick={onClick} type="button" data-testid={dataTestid} {title}>
    {displayKey}
  </button>
{:else}
  <span class={classes} style={resolvedStyle} data-testid={dataTestid} {title}>
    {displayKey}
  </span>
{/if}
