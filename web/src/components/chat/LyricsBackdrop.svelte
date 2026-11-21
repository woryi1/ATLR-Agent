<script lang="ts">
  // Background lyrics, sharing the visualiser's layer: both sit behind the
  // transcript (z-index 0, no pointer events).
  //
  // The block is a fixed-height window onto a vertical track of lines. The whole
  // track slides so the active line stays centred, which is what makes a change
  // read as a scroll instead of the text hard-swapping in place; neighbours
  // shrink and fade with distance. Size, spacing and colour come from settings.
  import { musicLyrics, musicPosition, vizSettings } from '../../lib/music'

  const activeIndex = $derived.by(() => {
    const pos = $musicPosition
    const lines = $musicLyrics
    // Last line whose start has passed — `to` is ignored on purpose. During the
    // gap between two lines the previous line is still what is on screen (the
    // sidebar player behaves the same way); requiring pos <= to made the whole
    // layer disappear in every gap.
    let idx = -1
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].from <= pos) idx = i
      else break
    }
    return idx
  })

  // Every line stays in the DOM (a song is ~100 rows, not thousands) so the
  // track can translate continuously; the rows that actually change style are
  // within a few of the active one, which keeps a tick cheap.
  const rows = $derived($musicLyrics.map((line, i) => ({ text: line.text.trim(), i })))
  const visible = $derived($vizSettings.lyrics && rows.length > 0 && activeIndex >= 0)

  function dist(i: number): number {
    const d = i - activeIndex
    return Math.max(-4, Math.min(4, d))
  }

  const color = $derived(
    $vizSettings.lyricsColor || ($vizSettings.white ? '#ffffff' : 'var(--blue-5)'),
  )
</script>

{#if visible}
  <div
    class="lyr-backdrop"
    style="color:{color};--gap:{$vizSettings.lyricsGap}px;--size:{$vizSettings.lyricsSize}px;--row:calc(var(--size) * 1.4 + var(--gap));--i:{activeIndex}"
    aria-hidden="true"
  >
    <div class="lyr-track">
      {#each rows as row (row.i)}
        <div class="lyr-line" data-d={dist(row.i)}>{row.text || '♪'}</div>
      {/each}
    </div>
  </div>
{/if}

<style>
  .lyr-backdrop {
    position: absolute;
    left: 50%;
    /* Slightly below the vertical centre, tall enough to read as a backdrop. */
    top: 52%;
    transform: translate(-50%, -50%);
    width: min(880px, 72%);
    height: calc(var(--row) * 7);
    overflow: hidden;
    z-index: 0;
    pointer-events: none;
    text-align: center;
    font-weight: 600;
    letter-spacing: 0.02em;
    /* Rows enter and leave through a soft edge instead of being cut off. */
    -webkit-mask-image: linear-gradient(to bottom, transparent 0%, #000 20%, #000 80%, transparent 100%);
    mask-image: linear-gradient(to bottom, transparent 0%, #000 20%, #000 80%, transparent 100%);
  }
  .lyr-track {
    position: absolute;
    left: 0;
    right: 0;
    top: 50%;
    /* Centre the active row (--i) inside the window; the slide is the scroll. */
    transform: translateY(calc(-1 * var(--row) * (var(--i) + 0.5)));
    transition: transform 520ms cubic-bezier(0.22, 1, 0.36, 1);
    will-change: transform;
  }
  .lyr-line {
    height: var(--row);
    display: flex;
    align-items: center;
    justify-content: center;
    max-width: 100%;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-size: calc(var(--size) * 0.26);
    opacity: 0.06;
    transition: font-size 480ms cubic-bezier(0.22, 1, 0.36, 1), opacity 480ms ease;
  }
  .lyr-line[data-d='0'] { font-size: var(--size); opacity: 0.72; }
  .lyr-line[data-d='-1'], .lyr-line[data-d='1'] { font-size: calc(var(--size) * 0.64); opacity: 0.38; }
  .lyr-line[data-d='-2'], .lyr-line[data-d='2'] { font-size: calc(var(--size) * 0.41); opacity: 0.18; }
  .lyr-line[data-d='-3'], .lyr-line[data-d='3'] { font-size: calc(var(--size) * 0.32); opacity: 0.10; }
  @media (max-width: 1100px) {
    /* Same breakpoint as the spectrum columns: not enough gutter to carry the
       watermark without crowding the transcript. */
    .lyr-backdrop { display: none; }
  }
  @media (prefers-reduced-motion: reduce) {
    .lyr-track, .lyr-line { transition: none; }
  }
</style>
