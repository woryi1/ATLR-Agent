<script lang="ts">
  // Background lyrics, sharing the visualiser's layer: both sit behind the
  // transcript (z-index 0, no pointer events). The window is the current line
  // plus two neighbours on either side, so the change from one line to the next
  // is a scroll rather than a single line swapping text in place.
  //
  // Size falls off away from the centre (largest = current line), the whole
  // block is anchored above the vertical centre, and the colour follows the
  // spectrum columns (theme accent, or white when that option is on).
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

  // Three neighbours on either side, empty lines included (rendered as a note).
  const windowLines = $derived.by(() => {
    const idx = activeIndex
    const lines = $musicLyrics
    if (idx < 0) return [] as { text: string; d: number }[]
    const out: { text: string; d: number }[] = []
    for (let d = -3; d <= 3; d++) {
      const line = lines[idx + d]
      if (line) out.push({ text: line.text.trim(), d })
    }
    return out
  })

  const color = $derived(
    $vizSettings.lyricsColor || ($vizSettings.white ? '#ffffff' : 'var(--blue-5)'),
  )
</script>

{#if $vizSettings.lyrics && windowLines.length > 0}
  <div
    class="lyr-backdrop"
    style="color:{color};--gap:{$vizSettings.lyricsGap}px;--size:{$vizSettings.lyricsSize}px"
    aria-hidden="true"
  >
    {#each windowLines as line (line.d)}
      {#key `${line.d}:${line.text}`}
        <div class="lyr-line" data-d={line.d}>{line.text || '♪'}</div>
      {/key}
    {/each}
  </div>
{/if}

<style>
  .lyr-backdrop {
    position: absolute;
    left: 50%;
    /* Slightly below the vertical centre, with enough spread that the block
       reads as a full-height backdrop rather than a small caption. */
    top: 52%;
    transform: translate(-50%, -50%);
    width: min(880px, 72%);
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--gap);
    z-index: 0;
    pointer-events: none;
    text-align: center;
    font-weight: 600;
    letter-spacing: 0.02em;
    filter: drop-shadow(0 2px 16px rgba(0, 0, 0, 0.14));
  }
  .lyr-line {
    max-width: 100%;
    line-height: 1.5;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    animation: lyr-in 320ms ease both;
    transition: font-size 320ms ease, opacity 320ms ease;
  }
  .lyr-line[data-d='0'] { font-size: var(--size); opacity: 0.72; }
  .lyr-line[data-d='-1'], .lyr-line[data-d='1'] { font-size: calc(var(--size) * 0.64); opacity: 0.38; }
  .lyr-line[data-d='-2'], .lyr-line[data-d='2'] { font-size: calc(var(--size) * 0.41); opacity: 0.18; }
  .lyr-line[data-d='-3'], .lyr-line[data-d='3'] { font-size: calc(var(--size) * 0.32); opacity: 0.10; }
  @keyframes lyr-in {
    from { opacity: 0; transform: translateY(5px); }
  }
  @media (max-width: 1100px) {
    /* Same breakpoint as the spectrum columns: not enough gutter to carry the
       watermark without crowding the transcript. */
    .lyr-backdrop { display: none; }
  }
  @media (prefers-reduced-motion: reduce) {
    .lyr-line { animation: none; transition: none; }
  }
</style>
