<!--
  The Super unlock animation.

  A full-screen video that plays the moment Super mode is switched on (and
  once on first launch, as the theme animation). Rules the user set:

    - it opens on the video's FIRST SECOND — that frame is held as the poster,
      so the animation starts from a still image, not from a black void;
    - a click or the spacebar starts it; it is never auto-playing on its own;
    - it plays ONCE and disappears when it ends;
    - no player controls of any kind — it must not read as a browser or a
      video player, just as an animation over the app;
    - it sits above everything else.

  The video element is deliberately bare: no `controls`, no download button,
  no fullscreen affordance, and the context menu is suppressed. The <video> is
  mounted as soon as the overlay opens (behind the poster) so it can preload
  and seek to second one before the user clicks — pressing play then starts
  exactly on the frame the poster showed, with no jump back to 0.
-->
<script lang="ts">
  import { tick as nextTick } from 'svelte'
  import { fade } from 'svelte/transition'
  import { cubicOut } from 'svelte/easing'
  import { prefersReducedMotion } from 'svelte/motion'
  import { superUnlock } from '../../lib/stores'
  import { t } from '../../lib/i18n'

  const VIDEO = '/super/unlock.mp4'
  const POSTER = '/super/unlock-poster.webp'
  // The video opens on its first second: the poster is that frame, and
  // playback starts from the same position.
  const START_AT = 1.0

  let videoEl = $state<HTMLVideoElement | null>(null)
  let active = $state(false)
  let mode = $state<'super' | 'intro'>('super')
  let playing = $state(false)
  let token = 0

  // A new request (Super switched on, or the first-launch intro) opens the
  // overlay on the poster frame.
  $effect(() => {
    const u = $superUnlock
    if (!u) return
    if (u.token === token) return
    token = u.token
    mode = u.mode
    active = true
    playing = false
    taps = 0
    if (tapTimer) { clearTimeout(tapTimer); tapTimer = null }
    // Seek as soon as the element exists; metadata may not be there yet, so
    // the load listeners below re-seek once it is.
    void nextTick().then(() => seekStart())
  })

  function seekStart(): void {
    const v = videoEl
    if (!v) return
    try {
      if (v.readyState >= 1 && Math.abs(v.currentTime - START_AT) > 0.05) {
        v.currentTime = START_AT
      }
    } catch { /* seeking before metadata is fine to skip */ }
  }

  // Preload and park the playhead on second one while the poster is showing.
  $effect(() => {
    const v = videoEl
    if (!v) return
    const onMeta = () => seekStart()
    v.addEventListener('loadedmetadata', onMeta)
    v.addEventListener('loadeddata', onMeta)
    seekStart()
    return () => {
      v.removeEventListener('loadedmetadata', onMeta)
      v.removeEventListener('loadeddata', onMeta)
    }
  })

  function start(): void {
    if (playing || !active) return
    const v = videoEl
    if (!v) return
    playing = true
    const go = () => {
      // The click IS the user gesture, so autoplay normally goes through.
      const p = v.play()
      if (p && typeof p.catch === 'function') {
        p.catch(() => { playing = false })
      }
    }
    // Seek first when the playhead is not already on the opening frame, and
    // only play after the seek lands — otherwise the first frames flash by.
    if (v.readyState >= 1 && Math.abs(v.currentTime - START_AT) > 0.15) {
      const onSeeked = () => {
        v.removeEventListener('seeked', onSeeked)
        go()
      }
      v.addEventListener('seeked', onSeeked)
      try {
        v.currentTime = START_AT
      } catch {
        v.removeEventListener('seeked', onSeeked)
        go()
      }
    } else {
      go()
    }
  }

  function finish(): void {
    // Stop and release the media before the overlay unmounts. Dropping the node
    // alone leaves the decoder, buffer and audio graph alive until GC; clearing
    // the source is what actually frees them, same as the wallpaper layer.
    const v = videoEl
    if (v) {
      try { v.pause() } catch { /* already gone */ }
      try {
        v.removeAttribute('src')
        v.load()
      } catch { /* ignore */ }
    }
    playing = false
    active = false
  }

  function onKeydown(e: KeyboardEvent): void {
    if (!active) return
    if (e.code === 'Space' || e.key === ' ' || e.key === 'Enter') {
      e.preventDefault()
      e.stopPropagation()
      tap()
    }
    if (e.key === 'Escape' && !playing) finish()
  }

  // A click (or Space / Enter) starts the animation; three taps in quick
  // succession while it plays skip it — but only for the Super reward, the
  // one-off intro is watched once and is not skippable.
  const SKIP_TAPS = 3
  const SKIP_WINDOW_MS = 1500
  let taps = 0
  let tapTimer: ReturnType<typeof setTimeout> | null = null

  function tap(): void {
    if (!active) return
    if (!playing) {
      start()
      return
    }
    if (mode !== 'super') return
    taps++
    if (tapTimer) clearTimeout(tapTimer)
    tapTimer = setTimeout(() => { taps = 0; tapTimer = null }, SKIP_WINDOW_MS)
    if (taps >= SKIP_TAPS) {
      taps = 0
      if (tapTimer) { clearTimeout(tapTimer); tapTimer = null }
      finish()
    }
  }

  let reduce = $derived(prefersReducedMotion.current)
</script>

<svelte:window onkeydown={onKeydown} />

{#if active}
  <div
    class="super-unlock"
    role="presentation"
    onclick={tap}
    oncontextmenu={(e) => e.preventDefault()}
    transition:fade={{ duration: reduce ? 0 : 240, easing: cubicOut }}
  >
    <!-- Mounted only while the overlay is active. preload=metadata keeps the
         7 MB file OFF the wire until the user actually presses play; the seek
         to second one fetches just that range. Never shown until playback
         starts. -->
    <!-- svelte-ignore a11y_media_has_caption -->
    <video
      bind:this={videoEl}
      class="super-unlock-video"
      class:visible={playing}
      src={VIDEO}
      preload="metadata"
      playsinline
      onended={finish}
      onerror={() => { /* no codec: close quietly rather than a broken frame */ finish() }}
    ></video>
    {#if !playing}
      <!-- The poster stands in for the video until the user starts it. -->
      <img class="super-unlock-poster" src={POSTER} alt="" draggable="false" />
      <div class="super-unlock-hint" aria-hidden="true">
        <span class="super-unlock-dot"></span>
        <span class="super-unlock-hint-text">{mode === 'intro' ? $t('super.start') : $t('super.enable')}</span>
      </div>
    {/if}
  </div>
{/if}

<style>
  /* Above every layer (the menus, the dialogs, the artwork). */
  .super-unlock {
    position: fixed;
    inset: 0;
    z-index: 9999;
    display: flex;
    align-items: center;
    justify-content: center;
    background: #000;
    cursor: pointer;
    user-select: none;
  }
  .super-unlock-video {
    position: absolute;
    inset: 0;
    width: 100%;
    height: 100%;
    object-fit: contain;
    background: #000;
    /* Hidden until playback: opacity alone would let a composited video
       surface bleed through the poster on some WebView2 builds, so it is
       visibility-hidden as well. Once visible it is fully opaque. */
    opacity: 0;
    visibility: hidden;
    pointer-events: none;
    /* The app must never show through the animation. */
    isolation: isolate;
  }
  .super-unlock-video.visible { opacity: 1; visibility: visible; }
  .super-unlock-poster {
    position: relative;
    width: 100%;
    height: 100%;
    object-fit: contain;
    background: #000;
    /* An opacity pulse read as "the overlay is see-through"; breathe with a
       barely-there scale instead so the still frame stays fully solid. */
    animation: super-unlock-breathe 3.2s ease-in-out infinite;
  }
  /* A quiet invitation, not a player control: a pulsing dot and a line of
     copy. Nothing that says "video". */
  .super-unlock-hint {
    position: absolute;
    bottom: 8vh;
    left: 50%;
    transform: translateX(-50%);
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 18px;
    border-radius: 999px;
    background: rgba(255, 255, 255, 0.08);
    color: rgba(255, 255, 255, 0.82);
    font-size: 13px;
    letter-spacing: 0.04em;
    backdrop-filter: blur(6px);
  }
  .super-unlock-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: #7cc4ff;
    animation: super-unlock-pulse 1.6s ease-in-out infinite;
  }
  @keyframes super-unlock-breathe {
    0%, 100% { transform: scale(1); }
    50%      { transform: scale(1.006); }
  }
  @keyframes super-unlock-pulse {
    0%, 100% { opacity: 0.35; transform: scale(0.85); }
    50%      { opacity: 1;    transform: scale(1.15); }
  }
  @media (prefers-reduced-motion: reduce) {
    .super-unlock-poster { animation: none; }
    .super-unlock-dot { animation: none; }
  }
</style>
