<script lang="ts">
  // Two thick, square-ended spectrum columns pinned to the left and right edges
  // of the workspace (the pane, not the sidebar), sitting behind the transcript
  // so messages stay readable on top. Left column = left channel, right column =
  // right channel: a mono track leaves the right column still, by construction.
  import { onMount } from 'svelte'
  import { readVizBands, vizSettings } from '../../lib/music'

  const BANDS = 22
  let left = $state<number[]>(Array(BANDS).fill(0))
  let right = $state<number[]>(Array(BANDS).fill(0))
  let live = $state(false)

  const color = $derived($vizSettings.white ? '#ffffff' : 'var(--blue-5)')
  // Idle columns stay visible but recede, so the gutter does not look broken.
  const alpha = $derived(live ? $vizSettings.opacity : $vizSettings.opacity * 0.4)

  onMount(() => {
    // Fast attack, slowish release: a beat punches the bar out immediately and
    // it settles over a few frames instead of snapping back (that snap is what
    // made it read like a volume meter).
    const held = Array(BANDS * 2).fill(0)
    let raf = 0
    const tick = () => {
      const bands = readVizBands()
      const nextLeft: number[] = []
      const nextRight: number[] = []
      for (let i = 0; i < BANDS; i++) {
        const l = (bands.left[i] ?? 0) / 255
        const r = (bands.right[i] ?? 0) / 255
        held[i] = Math.max(l, held[i] * 0.9)
        held[i + BANDS] = Math.max(r, held[i + BANDS] * 0.9)
        nextLeft.push(held[i])
        nextRight.push(held[i + BANDS])
      }
      left = nextLeft
      right = nextRight
      live = bands.live
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  })
</script>

{#if $vizSettings.enabled}
  <div
    class="viz viz-left"
    style="color:{color};width:{$vizSettings.width}px;opacity:{alpha}"
    aria-hidden="true"
  >
    {#each left as value}
      <span class="bar" style="width:{Math.round(value * 100)}%"></span>
    {/each}
  </div>
  <div
    class="viz viz-right"
    style="color:{color};width:{$vizSettings.width}px;opacity:{alpha}"
    aria-hidden="true"
  >
    {#each right as value}
      <span class="bar" style="width:{Math.round(value * 100)}%"></span>
    {/each}
  </div>
{/if}

<style>
  /* Height is a third of the viewport, centred; the bars share it evenly so the
     column reads as a block of thick squares rather than a hairline. */
  .viz {
    position: absolute; top: 50%; transform: translateY(-50%);
    height: 33vh;
    display: flex; flex-direction: column; justify-content: space-between;
    z-index: 0; pointer-events: none;
    transition: opacity 320ms ease;
  }
  .viz-left { left: 14px; align-items: flex-start; }
  /* Clear of the timeline rail: the rail sits at max(22px, (100% - content)/4),
     so the right column ends 30px further left. */
  .viz-right {
    right: max(52px, calc((100% - var(--chat-content-max-width)) / 4 + 30px));
    align-items: flex-end;
  }
  .bar {
    display: block; height: 8px; border-radius: 2px;
    background: currentColor;
    transition: width 90ms linear;
  }
  @media (max-width: 1100px) {
    /* Not enough gutter for two columns plus the transcript. */
    .viz { display: none; }
  }
</style>
