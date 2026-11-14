<!--
  Reasoning-tier artwork overlay.

  The character art for the CURRENT reasoning tier, floating in the middle of
  the screen while the model menu is open. It is decoration, not UI: it never
  takes a click (pointer-events: none) and it sits above the page but below
  the menus that opened it.

  Two rules the user set:
    - it shows while the model menu is expanded;
    - when Super mode is on, it stays on the main screen for 5 seconds after
      the menu closes (the reward lingers), then fades out.
-->
<script lang="ts">
  import { fade } from 'svelte/transition'
  import { cubicOut } from 'svelte/easing'
  import { prefersReducedMotion } from 'svelte/motion'
  import { effortArtFor } from '../../lib/stores'

  let { level = 'off', superOn = false, open = false }: {
    level?: string
    superOn?: boolean
    open?: boolean
  } = $props()

  // The Super reward lingers: when the menu closes with Super on, hold the
  // artwork for five seconds before letting it go.
  const LINGER_MS = 5000
  let lingering = $state(false)
  let lingerTimer: ReturnType<typeof setTimeout> | null = null

  let visible = $derived(open || lingering)

  // Each level has a list of interchangeable pictures. Re-roll when the menu
  // opens AND whenever the level changes, so working the slider (off → high →
  // max, …) never keeps showing the same drawing.
  let roll = $state(Math.random())
  let lastOpen = false
  let lastLevel = ''
  $effect(() => {
    if (open && (!lastOpen || level !== lastLevel)) roll = Math.random()
    lastOpen = open
    lastLevel = level
  })

  let src = $derived(effortArtFor(level, superOn, roll))

  $effect(() => {
    // Read the dependencies so the effect re-runs when either changes.
    const wasOpen = open
    const isSuper = superOn
    if (wasOpen) {
      // Menu is open: show it now, cancel any pending linger.
      if (lingerTimer) { clearTimeout(lingerTimer); lingerTimer = null }
      lingering = false
      return
    }
    // Menu closed. Only Super earns the linger.
    if (!isSuper) {
      lingering = false
      return
    }
    lingering = true
    if (lingerTimer) clearTimeout(lingerTimer)
    lingerTimer = setTimeout(() => { lingering = false }, LINGER_MS)
    return () => { if (lingerTimer) { clearTimeout(lingerTimer); lingerTimer = null } }
  })

  let reduce = $derived(prefersReducedMotion.current)
</script>

{#if visible}
  <div
    class="effort-art"
    class:effort-art-linger={!open && lingering}
    data-tier={level}
    aria-hidden="true"
    transition:fade={{ duration: reduce ? 0 : 320, easing: cubicOut }}
  >
    <!-- Mounted only while visible: the browser fetches the artwork on demand
         and the element (and its decoded bitmap) goes away with the overlay.
         decoding=async keeps a 900px-tall decode off the paint critical path. -->
    <img class="effort-art-img" {src} alt="" draggable="false" decoding="async" />
  </div>
{/if}

<style>
  /* Fixed, centred, above the page — the artwork is the subject, the UI is
     the backdrop. pointer-events: none keeps every click on the app. */
  .effort-art {
    position: fixed;
    inset: 0;
    z-index: 40;
    display: flex;
    align-items: center;
    justify-content: center;
    pointer-events: none;
  }
  .effort-art-img {
    height: min(78vh, 900px);
    width: auto;
    max-width: 92vw;
    object-fit: contain;
    /* Off-centre by design: lifted a little and pushed right, so the art sits
       beside the model menu instead of dead-centre on top of it. Margins do
       the offset — `transform` belongs to the entrance animation. */
    margin: 0 0 10vh 12vw;
    filter: drop-shadow(0 18px 48px rgba(0, 0, 0, 0.45));
    animation: effort-art-in 420ms cubic-bezier(0.22, 1, 0.36, 1);
  }
  /* The lingering copy after the menu closed: it stays where it is, just a
     little softer, so it reads as "the reward stays a moment". */
  .effort-art-linger .effort-art-img {
    opacity: 0.92;
    animation: effort-art-breathe 2.4s ease-in-out infinite;
  }
  @keyframes effort-art-in {
    from { opacity: 0; transform: translateY(14px) scale(0.97); }
    to   { opacity: 1; transform: none; }
  }
  @keyframes effort-art-breathe {
    0%, 100% { transform: scale(1); }
    50%      { transform: scale(1.012); }
  }
  @media (prefers-reduced-motion: reduce) {
    .effort-art-img { animation: none; }
    .effort-art-linger .effort-art-img { animation: none; }
  }
</style>
