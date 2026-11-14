<script lang="ts">
  // Reasoning-effort slider, shown in the model menu.
  //
  // A custom slider, not <input type="range">. The native control fought us:
  // its value was bound to a $derived index, so every drag frame wrote the
  // thumb's position back to the DOM a tick late — the thumb lagged the
  // pointer and the thing felt like it could not be dragged at all. Here the
  // thumb is a single element moved with `transform: translateX(...)`, which
  // the compositor animates on its own: the pointer sets the position, the
  // browser paints it on the next frame, and nothing ever writes a stale
  // value back.
  //
  // Dragging is CONTINUOUS. The pointer's pixel position owns the thumb; the
  // level is only resolved on release (snap to the nearest notch). Rounding
  // on every move is what made the old version feel like a series of clicks
  // instead of a slide.
  //
  // The notches are NOT hard-coded: they are the ladder the current model
  // declares (GET /api/config/endpoints → models[].reasoning_efforts). A
  // DeepSeek model declares off/high/max, so the rail has three notches; a
  // model that declares nothing falls back to the full six-level ladder.
  //
  // Write policy: dragging is local and free; the PATCH fires on release
  // (pointerup) — or on the last position of a keyboard step. A failed
  // commit leaves the server's value in place, so the thumb springs back to
  // it instead of claiming a level the server rejected.
  import { t } from '../../lib/i18n'

  let { value = 'off', levels = [], onCommit }: {
    value?: string
    // The model's declared ladder, ascending. Empty = undeclared → full ladder.
    levels?: string[]
    onCommit: (level: string) => Promise<boolean> | boolean
  } = $props()

  // Mirror of config.ReasoningLadder — the server can only ever send these, so
  // this is the ordering/clamping table, not a UI decision.
  const LADDER = ['off', 'low', 'medium', 'high', 'xhigh', 'max']
  const LABEL: Record<string, string> = {
    off: 'composer.effort_level.off',
    low: 'composer.effort_level.low',
    medium: 'composer.effort_level.medium',
    high: 'composer.effort_level.high',
    xhigh: 'composer.effort_level.xhigh',
    max: 'composer.effort_level.max',
  }

  // Model-declared names only: an unknown level falls back to its raw id, so a
  // future level needs no i18n key to render.
  function label(level: string): string {
    const key = LABEL[level]
    return key ? $t(key) : level
  }

  // The ladder actually rendered: the model's declaration, or the full ladder.
  let steps = $derived(levels.length ? levels : LADDER)

  // Nearest notch for a value the model may not offer (the session can carry a
  // level picked while another model was active). Falls back to the highest
  // declared notch at or below `value`, else the lowest.
  function nearest(list: string[], v: string): number {
    const exact = list.indexOf(v)
    if (exact >= 0) return exact
    const want = LADDER.indexOf(v)
    if (want < 0) return 0
    let best = 0
    for (let i = 0; i < list.length; i++) {
      if (LADDER.indexOf(list[i]) <= want) best = i
    }
    return best
  }

  const THUMB = 16

  let railEl = $state<HTMLDivElement | null>(null)
  let railW = $state(0)

  // While dragging, the pointer owns the position (raw pixels, not a notch).
  // The server's value is the authority the moment the drag ends.
  let dragPos = $state<number | null>(null)
  let dragging = $state(false)
  let busy = $state(false)

  // Where the thumb settles after release, kept until the PATCH resolves so
  // the thumb does not bounce back to the old notch during the round-trip.
  let snapPx = $state<number | null>(null)

  let idx = $derived(nearest(steps, value))
  let travel = $derived(Math.max(1, railW - THUMB))
  let baseX = $derived((idx / Math.max(1, steps.length - 1)) * travel)
  let overrideX = $derived(dragPos ?? snapPx)
  let thumbX = $derived(overrideX ?? baseX)

  // The level the label previews while the pointer is down or the write is in
  // flight: the one the thumb snaps to.
  let previewIdx = $derived(
    overrideX === null
      ? idx
      : Math.round((overrideX / travel) * Math.max(0, steps.length - 1)),
  )

  function pxFromEvent(e: PointerEvent): number {
    const el = railEl
    if (!el) return 0
    const r = el.getBoundingClientRect()
    return Math.min(travel, Math.max(0, e.clientX - r.left - THUMB / 2))
  }

  function onPointerDown(e: PointerEvent): void {
    if (busy || steps.length <= 1) return
    // Capture the pointer so the drag keeps going even when the cursor
    // leaves the rail.
    ;(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId)
    dragging = true
    // Jump the thumb under the pointer, then follow it continuously.
    dragPos = pxFromEvent(e)
    e.preventDefault()
  }

  function onPointerMove(e: PointerEvent): void {
    if (!dragging) return
    // One write per move, straight into the transform — no rounding, no rAF
    // batching. The compositor keeps this on the pointer.
    dragPos = pxFromEvent(e)
  }

  function endDrag(e: PointerEvent): void {
    if (!dragging) return
    const n = Math.max(0, steps.length - 1)
    const picked = dragPos === null ? idx : Math.round((dragPos / travel) * n)
    const clamped = Math.min(steps.length - 1, Math.max(0, picked))
    dragging = false
    // Dropping the pixel override lets the CSS transition carry the thumb
    // onto the notch — a short settle instead of a hard teleport. The settled
    // pixel stays authoritative until the write resolves.
    dragPos = null
    snapPx = (clamped / Math.max(1, n)) * travel
    void commit(steps[clamped]).finally(() => { snapPx = null })
  }

  async function commit(level: string | undefined): Promise<void> {
    if (!level || busy) return
    if (level === value) return
    busy = true
    try {
      // false = the PATCH failed (pickReasoning already toasted); the value
      // prop stays put and the thumb springs back.
      await onCommit(level)
    } catch {
      /* same: the prop is the truth */
    } finally {
      busy = false
    }
  }

  function onKeydown(e: KeyboardEvent): void {
    if (busy) return
    const n = steps.length
    if (e.key === 'ArrowLeft' || e.key === 'ArrowDown') {
      e.preventDefault()
      void commit(steps[Math.max(0, nearest(steps, value) - 1)])
    } else if (e.key === 'ArrowRight' || e.key === 'ArrowUp') {
      e.preventDefault()
      void commit(steps[Math.min(n - 1, nearest(steps, value) + 1)])
    }
  }

  // The rail's width decides the travel distance; keep it live so a resize
  // (window drag, sidebar collapse) does not leave the thumb off the track.
  $effect(() => {
    const el = railEl
    if (!el) return
    const ro = new ResizeObserver(() => { railW = el.clientWidth })
    ro.observe(el)
    railW = el.clientWidth
    return () => ro.disconnect()
  })
</script>

<div class="effort" class:dragging>
  <div class="es-head">
    <span class="es-title">{$t('composer.effort')}</span>
    <span class="es-level">{label(steps[previewIdx] ?? value)}</span>
  </div>
  <div
    class="es-rail"
    class:active={dragging}
    bind:this={railEl}
    role="slider"
    tabindex="0"
    aria-label={$t('composer.effort')}
    aria-valuemin={0}
    aria-valuemax={Math.max(0, steps.length - 1)}
    aria-valuenow={previewIdx}
    aria-valuetext={label(steps[previewIdx] ?? value)}
    aria-disabled={busy}
    onpointerdown={onPointerDown}
    onpointermove={onPointerMove}
    onpointerup={endDrag}
    onpointercancel={endDrag}
    onkeydown={onKeydown}
  >
    <div class="es-track">
      <div class="es-fill" style={`--fill:${(thumbX / travel) * 100}%`}></div>
    </div>
    <!-- Absolutely positioned: the labels must never take part in layout. -->
    <div class="es-ticks" aria-hidden="true">
      {#each steps as l, i}
        <span
          class:on={i === previewIdx}
          class:edge-l={i === 0}
          class:edge-r={i === steps.length - 1}
          style={`left:${(i / Math.max(1, steps.length - 1)) * 100}%`}
        >{label(l)}</span>
      {/each}
    </div>
    <div class="es-thumb" style={`transform: translate3d(${thumbX}px, 0, 0)`}></div>
  </div>
</div>

<style>
.effort { display: flex; flex-direction: column; gap: 5px; }
.es-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.es-title { font-size: 11px; color: var(--text-tertiary); }
.es-level {
  font-size: 11.5px; font-weight: 600; color: var(--text);
  font-family: var(--font-mono);
}

/* A taller rail = a bigger grab target. 24px total, the tick row hugs its
   bottom edge. */
.es-rail {
  position: relative;
  height: 24px;
  cursor: grab;
  touch-action: none;
  outline: none;
  -webkit-user-select: none;
  user-select: none;
}
.es-rail.active { cursor: grabbing; }
.es-rail:focus-visible .es-thumb { box-shadow: 0 0 0 3px var(--focus-ring); }
.es-track {
  position: absolute; left: 0; right: 0; top: 9px;
  height: 6px; border-radius: 999px;
  background: var(--border-secondary);
  overflow: hidden;
}
.es-fill {
  height: 100%;
  width: var(--fill, 0%);
  background: var(--blue-6);
  /* Only the fill eases; the thumb itself is transform-driven and must not
     lag the pointer. No easing while the pointer is down. */
  transition: width 140ms ease-out;
}
.es-rail.active .es-fill { transition: none; }
/* The thumb: moved with translate3d only, so the compositor animates it and
   it tracks the pointer exactly. The transition is the "settle onto the
   notch" after release — it is switched off mid-drag. */
.es-thumb {
  position: absolute;
  top: 4px;
  left: 0;
  width: 16px; height: 16px;
  border-radius: 50%;
  background: #fff;
  border: 2px solid var(--blue-6);
  box-shadow: 0 1px 3px rgba(15, 23, 42, 0.22);
  will-change: transform;
  pointer-events: none;
  transform: translate3d(0, 0, 0);
  transition: transform 180ms cubic-bezier(0.22, 1, 0.36, 1);
}
.es-rail.active .es-thumb { transition: none; }
.es-ticks {
  position: absolute; left: 0; right: 0; top: 17px; height: 7px;
  font-size: 9.5px; color: var(--text-tertiary); line-height: 1;
  pointer-events: none;
}
.es-ticks span {
  position: absolute; transform: translateX(-50%);
  white-space: nowrap;
}
/* The end labels hug the track instead of hanging half outside it. */
.es-ticks span.edge-l { transform: none; }
.es-ticks span.edge-r { transform: translateX(-100%); }
.es-ticks .on { color: var(--text); font-weight: 600; }

/* Svelte's JS-driven transitions ignore CSS media queries; this block is the
   static-CSS half. */
@media (prefers-reduced-motion: reduce) {
  .effort, .effort * { transition-duration: 0.01ms !important; animation-duration: 0.01ms !important; }
}
</style>
