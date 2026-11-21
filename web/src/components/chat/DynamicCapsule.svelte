<script lang="ts">
  // Dynamic capsule, sitting on the window-controls axis at the top centre.
  // Collapsed it shows the current lyric line (a musical note during an
  // instrumental gap, or the date when nothing is playing); expanded it shows
  // the track, the lyric neighbourhood, a seek bar and a calendar.
  //
  // Collapsed by default. A click expands it and starts an 8s auto-collapse,
  // paused while the pointer is over it; clicking anywhere outside collapses.
  import { onDestroy, onMount, untrack } from 'svelte'
  import { get } from 'svelte/store'
  import {
    musicLyrics, musicPosition, musicDuration, musicNow, musicPlaying,
    toggleMusic, nextMusic, prevMusic, seekMusic, proxyImg, vizSettings,
    pauseMusic, resumeMusic,
  } from '../../lib/music'
  import { locale, t } from '../../lib/i18n'
  import { capsuleNotice, clearCapsuleNotice, setActiveSession, view, activeSessionId, sessions, tasks, createNewSession, settingsModalOpen } from '../../lib/stores'
  import { videoLog } from '../../lib/debugLog'

  let open = $state(false)
  let hovered = $state(false)
  let monthOffset = $state(0)
  let now = $state(new Date())
  let timer: ReturnType<typeof setTimeout> | null = null
  let rootEl = $state<HTMLElement | null>(null)
  // Video mode for the lyric area. Off by default; the stream is requested only
  // when the user switches it on, and released when the panel closes or the
  // track changes.
  let showVideo = $state(false)
  let videoEl = $state<HTMLVideoElement | null>(null)
  let videoError = $state('')

  const clock = setInterval(() => { now = new Date() }, 30_000)

  // Last line whose start has passed. Deliberately ignores `to`: the gap
  // between two sung lines is still that line's time on screen, which is what
  // the sidebar player shows and what the user expects here. Using `to` made
  // both this capsule and the background layer vanish in every gap.
  const activeIndex = $derived.by(() => {
    const pos = $musicPosition
    const lines = $musicLyrics
    let idx = -1
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].from <= pos) idx = i
      else break
    }
    return idx
  })

  // Three neighbours on either side. Empty lines stay in the window (rendered
  // as a note) rather than shortening it.
  const preview = $derived.by(() => {
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

  const currentLine = $derived(preview.find((l) => l.d === 0)?.text ?? '')
  const color = $derived(
    $vizSettings.lyricsColor || ($vizSettings.white ? '#ffffff' : 'var(--blue-5)'),
  )
  // Formatters are rebuilt only when the language changes, not on every clock
  // tick: constructing Intl.DateTimeFormat is comparatively expensive.
  const dateFmt = $derived(new Intl.DateTimeFormat($locale === 'zh' ? 'zh-CN' : 'en', {
    month: 'short',
    day: 'numeric',
  }))
  const clockFmt = $derived(new Intl.DateTimeFormat($locale === 'zh' ? 'zh-CN' : 'en', {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }))
  const dateLabel = $derived(dateFmt.format(now))
  const clockLabel = $derived(clockFmt.format(now))
  const collapsedLabel = $derived(currentLine || ($musicNow ? '♪' : dateLabel))

  // The session the reminder belongs to, for its subtitle.
  const noticeTitle = $derived((() => {
    const n = $capsuleNotice
    if (!n) return ''
    return $sessions.find((s) => s.id === n.sessionId)?.title ?? ''
  })())

  // Scheduled runs due today, first three — a glance at what the capsule can
  // still be useful for besides lyrics and the calendar.
  const todayTasks = $derived($tasks.filter((task) => {
    if (!task.nextRun) return false
    const d = new Date(task.nextRun)
    if (Number.isNaN(d.getTime())) return false
    return d.getFullYear() === now.getFullYear()
      && d.getMonth() === now.getMonth()
      && d.getDate() === now.getDate()
  }).slice(0, 3))

  function taskTime(iso: string): string {
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? '' : clockFmt.format(d)
  }

  function goto(target: 'new' | 'tasks' | 'settings'): void {
    if (target === 'new') {
      createNewSession()
      view.set('chat')
    } else if (target === 'tasks') {
      view.set('tasks')
    } else {
      settingsModalOpen.set(true)
    }
    open = false
    closeVideo()
    clearTimer()
  }

  // Snapshot of the media element for the debug log. Never throws.
  function videoState(): Record<string, unknown> {
    const v = videoEl
    if (!v) return { video: null }
    let buffered = ''
    try {
      const b = v.buffered
      buffered = b.length ? `${b.start(b.length - 1).toFixed(2)}-${b.end(b.length - 1).toFixed(2)}` : ''
    } catch { /* ignore */ }
    return {
      readyState: v.readyState,
      networkState: v.networkState,
      paused: v.paused,
      currentTime: Number(v.currentTime.toFixed(2)),
      duration: Number.isFinite(v.duration) ? Number(v.duration.toFixed(2)) : -1,
      buffered,
      videoWidth: v.videoWidth,
      videoHeight: v.videoHeight,
      errorCode: v.error?.code ?? 0,
      errorMessage: v.error?.message ?? '',
      musicPos: Number(get(musicPosition).toFixed(2)),
      musicPlaying: get(musicPlaying),
    }
  }

  // Release the media element for real: dropping the node alone can leave the
  // decoder and its buffers alive until GC. The music player owns the sound in
  // this mode, so there is nothing to hand back.
  function closeVideo(): void {
    const v = videoEl
    if (!v) return
    videoLog('video.close', { bvid: $musicNow?.bvid ?? '', ...videoState() })
    quietly(() => {
      try { v.pause() } catch { /* already gone */ }
      try {
        v.removeAttribute('src')
        v.load()
      } catch { /* ignore */ }
    })
  }

  // Media events are dispatched asynchronously, so a pause we trigger ourselves
  // (track switch, closing the player) would otherwise look like the user
  // pressing pause and stop the music. The window covers the queued events.
  let programmatic = 0
  function quietly(fn: () => void): void {
    programmatic++
    try { fn() } finally {
      setTimeout(() => { programmatic = Math.max(0, programmatic - 1) }, 400)
    }
  }

  function toggleVideo(): void {
    showVideo = !showVideo
    videoError = ''
    videoLog('video.toggle', {
      show: showVideo,
      bvid: $musicNow?.bvid ?? '',
      lyrics: $musicLyrics.length,
      musicPlaying: get(musicPlaying),
      musicPos: get(musicPosition),
      open,
    })
    if (showVideo) {
      // Entering video mode pins the panel open. The music keeps playing: the
      // DASH rendition is video-only, so it is the sound for this mode.
      clearTimer()
    } else {
      closeVideo()
      scheduleAutoClose()
    }
  }

  // The stream is video-only: the music player (same track) owns the sound, so
  // the picture is slaved to the audio clock. On metadata, align to the music
  // position and start only if the music is playing.
  function onVideoMeta(): void {
    const v = videoEl
    if (!v) return
    v.muted = true
    const pos = get(musicPosition)
    videoLog('video.metadata', { ...videoState(), seekTo: Number(pos.toFixed(2)) })
    try {
      if (pos > 0) v.currentTime = pos
    } catch (e) {
      videoLog('video.seek.failed', { error: String(e) })
    }
    if (!get(musicPlaying)) return
    v.play()
      .then(() => videoLog('video.play.ok', videoState()))
      .catch((e) => videoLog('video.play.rejected', {
        name: (e as Error)?.name ?? '',
        message: String((e as Error)?.message ?? e),
        ...videoState(),
      }))
  }

  /** Video and music are the same timeline; correct drift, never fight it. */
  function syncVideoToMusic(): void {
    const v = videoEl
    if (!v || v.seeking || v.ended) return
    const pos = get(musicPosition)
    if (Math.abs(v.currentTime - pos) > 0.5) {
      const from = v.currentTime
      try { v.currentTime = pos } catch { /* ignore */ }
      videoLog('video.sync.seek', { from: Number(from.toFixed(2)), to: Number(pos.toFixed(2)) })
    }
  }

  /** The capsule's seek bar drives both clocks, so they never split. */
  function seekBoth(seconds: number): void {
    seekMusic(seconds)
    const v = videoEl
    if (v && showVideo) {
      try { v.currentTime = seconds } catch { /* ignore */ }
    }
  }

  // Native controls must drive the music too, otherwise the picture pauses and
  // the sound keeps going.
  function onVideoPlay(): void {
    logVideoEvent('play')
    if (programmatic > 0) return
    if (!get(musicPlaying)) resumeMusic()
  }

  function onVideoPause(): void {
    logVideoEvent('pause')
    if (programmatic > 0) return
    if (get(musicPlaying)) pauseMusic()
  }

  function onVideoSeeked(): void {
    logVideoEvent('seeked')
    if (programmatic > 0) return
    const v = videoEl
    if (!v) return
    if (Math.abs(get(musicPosition) - v.currentTime) > 0.5) seekMusic(v.currentTime)
  }

  // The video carries no audio track; unmuting it would only stack a silent
  // control on top of the music. Keep it muted and say so in the log.
  function onVideoVolumeChange(): void {
    const v = videoEl
    if (v && !v.muted) {
      v.muted = true
      videoLog('video.muted.forced', videoState())
    }
  }

  function onVideoError(): void {
    const code = videoEl?.error?.code
    videoError = code ? `code ${code}` : 'unknown'
    videoLog('video.error', {
      code: code ?? 0,
      message: videoEl?.error?.message ?? '',
      ...videoState(),
    })
  }

  let lastProgressLog = 0
  function onVideoProgress(): void {
    const nowMs = Date.now()
    if (nowMs - lastProgressLog < 1000) return
    lastProgressLog = nowMs
    videoLog('video.progress', videoState())
  }

  function logVideoEvent(name: string): void {
    videoLog(`video.event.${name}`, videoState())
  }

  // Swap the source of the live element in place when the song changes. Svelte
  // has already pointed the src attribute at the new bvid; load() aborts the
  // previous request, frees its decoder buffers, and restarts the resource
  // selection, so the panel cannot keep showing the old frozen frame.
  function switchVideo(bvid: string): void {
    const v = videoEl
    if (!v) return
    videoError = ''
    videoLog('video.switch', { bvid, ...videoState() })
    quietly(() => {
      try { v.pause() } catch { /* already gone */ }
      try { v.currentTime = 0 } catch { /* ignore */ }
      try { v.load() } catch { /* ignore */ }
    })
  }

  // Closing the panel or changing track must stop the stream. untrack keeps
  // closeVideo's reads (videoEl, musicNow) OUT of these effects' dependencies:
  // without it, binding the media element itself re-ran an effect and cleared
  // the src the moment it was set. `pre` so the release happens while the
  // element is still bound — after the panel unmounts there is nothing left to
  // pause and the browser keeps the download alive until GC.
  $effect.pre(() => {
    if (!open) untrack(() => {
      closeVideo()
      // Leaving the panel exits video mode too. Keeping showVideo true remounted
      // the player on the next open and started a second audio source over the
      // music closeVideo had just resumed.
      showVideo = false
    })
  })

  // A track change follows into the open video. This used to call closeVideo()
  // unconditionally, which removed the src Svelte had just pointed at the new
  // bvid — the picture froze until the button was toggled off and on again.
  let lastVideoBvid = ''
  $effect(() => {
    const bvid = $musicNow?.bvid ?? ''
    if (bvid === lastVideoBvid) return
    lastVideoBvid = bvid
    untrack(() => {
      if (!showVideo || !bvid) {
        closeVideo()
        return
      }
      switchVideo(bvid)
    })
  })

  // The capsule's play/pause is the music transport; mirror it onto the picture.
  $effect(() => {
    const playing = $musicPlaying
    const v = videoEl
    if (!v || !showVideo) return
    if (playing && v.paused) void v.play().catch(() => {})
    else if (!playing && !v.paused) v.pause()
  })

  // Two independent decoders drift; nudge the picture back to the audio clock.
  $effect(() => {
    if (!showVideo) return
    const id = setInterval(() => {
      if (get(musicPlaying)) syncVideoToMusic()
    }, 2000)
    return () => clearInterval(id)
  })

  // A short two-note chime, deliberately different from the voice-input cue.
  // Synthesised so it needs no asset; a blocked AudioContext just stays silent.
  let audioCtx: AudioContext | null = null
  function playNoticeChime(): void {
    try {
      const Ctor = window.AudioContext
        ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
      if (!Ctor) return
      if (!audioCtx) audioCtx = new Ctor()
      if (audioCtx.state === 'suspended') void audioCtx.resume()
      const ctx = audioCtx
      const start = ctx.currentTime
      const notes = [880, 1174.66] // A5 → D6
      notes.forEach((freq, i) => {
        const osc = ctx.createOscillator()
        const gain = ctx.createGain()
        osc.type = 'sine'
        osc.frequency.value = freq
        const at = start + i * 0.11
        gain.gain.setValueAtTime(0.0001, at)
        gain.gain.exponentialRampToValueAtTime(0.05, at + 0.015)
        gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.22)
        osc.connect(gain)
        gain.connect(ctx.destination)
        osc.start(at)
        osc.stop(at + 0.24)
      })
    } catch { /* autoplay blocked: stays silent */ }
  }

  // Foreground reminder. A turn finishing or a pending confirmation pops the
  // capsule open for its 8s window; in the background the desktop toast covers
  // it, and the capsule just keeps the dot until the user comes back.
  $effect(() => {
    const n = $capsuleNotice
    if (!n) return
    const fg = typeof document !== 'undefined'
      && document.visibilityState === 'visible'
      && document.hasFocus()
    if (fg) {
      open = true
      playNoticeChime()
      scheduleAutoClose()
    }
  })

  const monthDate = $derived(new Date(now.getFullYear(), now.getMonth() + monthOffset, 1))
  const cells = $derived.by(() => {
    const y = monthDate.getFullYear()
    const m = monthDate.getMonth()
    const start = (new Date(y, m, 1).getDay() + 6) % 7
    const days = new Date(y, m + 1, 0).getDate()
    const out: (number | null)[] = []
    for (let i = 0; i < start; i++) out.push(null)
    for (let i = 1; i <= days; i++) out.push(i)
    return out
  })
  const weekdays = $derived.by(() => {
    const fmt = new Intl.DateTimeFormat($locale === 'zh' ? 'zh-CN' : 'en', { weekday: 'narrow' })
    return Array.from({ length: 7 }, (_, i) => fmt.format(new Date(2024, 0, 1 + i)))
  })
  const isToday = (day: number | null) => day !== null && monthOffset === 0 && day === now.getDate()

  function fmtTime(sec: number): string {
    if (!Number.isFinite(sec) || sec <= 0) return '0:00'
    const m = Math.floor(sec / 60)
    const s = Math.floor(sec % 60)
    return `${m}:${String(s).padStart(2, '0')}`
  }

  function clearTimer(): void {
    if (timer) {
      clearTimeout(timer)
      timer = null
    }
  }
  function scheduleAutoClose(): void {
    clearTimer()
    // Watching the video must not be interrupted by the idle timer: the panel
    // closing is what unmounted the <video> before it could show a frame.
    if (showVideo) return
    timer = setTimeout(() => {
      timer = null
      if (!hovered) {
        videoLog('capsule.autoclose', { showVideo })
        open = false
      }
    }, 8000)
  }
  function toggle(): void {
    videoLog('capsule.toggle', {
      opening: !open,
      showVideo,
      hasNotice: !!$capsuleNotice,
      bvid: $musicNow?.bvid ?? '',
    })
    // Acting on a reminder clears it, and jumps to the session it names when
    // that is not the one already on screen.
    const n = $capsuleNotice
    if (n) {
      clearCapsuleNotice()
      if (n.sessionId !== $activeSessionId) {
        view.set('chat')
        setActiveSession(n.sessionId)
      }
    }
    if (open) {
      open = false
      clearTimer()
      return
    }
    open = true
    scheduleAutoClose()
  }

  function onDocPointerDown(e: PointerEvent): void {
    if (!open) return
    if (rootEl && e.target instanceof Node && rootEl.contains(e.target)) return
    videoLog('capsule.outside_click', { showVideo })
    open = false
    clearTimer()
  }

  onMount(() => {
    document.addEventListener('pointerdown', onDocPointerDown, true)
    return () => document.removeEventListener('pointerdown', onDocPointerDown, true)
  })
  onDestroy(() => {
    clearTimer()
    clearInterval(clock)
  })
</script>

{#if $vizSettings.capsule}
  <div
    class="capsule-wrap"
    class:open
    bind:this={rootEl}
    style="--c:{color}"
    ondblclick={(e) => e.stopPropagation()}
    onmouseenter={() => {
      hovered = true
      clearTimer()
    }}
    onmouseleave={() => {
      hovered = false
      if (open) scheduleAutoClose()
    }}
  >
    <button
      type="button"
      class="capsule"
      class:alert={!!$capsuleNotice}
      aria-expanded={open}
      title={$capsuleNotice
        ? ($capsuleNotice.kind === 'done' ? $t('capsule.done') : $t('capsule.confirm'))
        : collapsedLabel}
      onclick={toggle}
    >
      <span class="capsule-dot" class:beat={$musicPlaying} class:notice={!!$capsuleNotice}></span>
      {#if $capsuleNotice}
        <span class="capsule-alert-text">
          {$capsuleNotice.kind === 'done' ? $t('capsule.done') : $t('capsule.confirm')}
        </span>
      {:else}
        <span class="capsule-label">{collapsedLabel}</span>
      {/if}
      <span class="capsule-chev">{open ? '︿' : '﹀'}</span>
    </button>

    {#if open}
      <div class="capsule-panel">
        {#if $musicNow}
          <div class="panel-song">
            <span class="song-cover">
              {#if $musicNow.cover}
                <img src={proxyImg($musicNow.cover)} alt="" />
              {:else}
                <iconify-icon icon="lucide:music" width="14"></iconify-icon>
              {/if}
            </span>
            <span class="song-meta">
              <span class="song-title" title={$musicNow.title}>{$musicNow.title}</span>
              <span class="song-artist">{$musicNow.artist}</span>
            </span>
            <span class="song-controls">
              <button type="button" aria-label="previous" onclick={() => prevMusic()}>
                <iconify-icon icon="lucide:skip-back" width="13"></iconify-icon>
              </button>
              <button type="button" class="song-play" aria-label="play/pause" onclick={() => toggleMusic()}>
                <iconify-icon icon={$musicPlaying ? 'lucide:pause' : 'lucide:play'} width="14"></iconify-icon>
              </button>
              <button type="button" aria-label="next" onclick={() => nextMusic()}>
                <iconify-icon icon="lucide:skip-forward" width="13"></iconify-icon>
              </button>
            </span>
          </div>
          <div class="panel-seek">
            <span>{fmtTime($musicPosition)}</span>
            <input
              type="range"
              min="0"
              max={$musicDuration || 0}
              step="0.5"
              value={$musicPosition}
              oninput={(e) => seekBoth(Number(e.currentTarget.value))}
            />
            <span>{fmtTime($musicDuration)}</span>
          </div>
        {/if}

        <div class="panel-lyrics" class:video={showVideo && $musicNow}>
          {#if showVideo && $musicNow}
            <video
              class="panel-video"
              bind:this={videoEl}
              src={`/api/music/video/${encodeURIComponent($musicNow.bvid)}`}
              poster={$musicNow.cover ? proxyImg($musicNow.cover) : undefined}
              playsinline
              muted
              preload="metadata"
              controls
              onloadstart={() => logVideoEvent('loadstart')}
              onloadedmetadata={onVideoMeta}
              onloadeddata={() => logVideoEvent('loadeddata')}
              oncanplay={() => logVideoEvent('canplay')}
              onplay={onVideoPlay}
              onplaying={() => logVideoEvent('playing')}
              onpause={onVideoPause}
              onwaiting={() => logVideoEvent('waiting')}
              onstalled={() => logVideoEvent('stalled')}
              onseeking={() => logVideoEvent('seeking')}
              onseeked={onVideoSeeked}
              onended={() => logVideoEvent('ended')}
              onemptied={() => logVideoEvent('emptied')}
              onabort={() => logVideoEvent('abort')}
              onvolumechange={onVideoVolumeChange}
              onprogress={onVideoProgress}
              onerror={onVideoError}
            ></video>
            {#if videoError}
              <div class="video-error">{$t('capsule.video_failed')} · {videoError}</div>
            {/if}
          {:else if preview.length}
            {#each preview as line (line.d)}
              <div class="pl" data-d={line.d}>{line.text || '♪'}</div>
            {/each}
          {:else}
            <div class="pl-empty">♪</div>
          {/if}
          {#if $musicNow}
            <button
              type="button"
              class="lyrics-video-btn"
              class:on={showVideo}
              title={showVideo ? $t('capsule.lyrics') : $t('capsule.video')}
              aria-label={showVideo ? $t('capsule.lyrics') : $t('capsule.video')}
              onclick={(e) => { e.stopPropagation(); toggleVideo() }}
            >
              <iconify-icon
                icon={showVideo ? 'lucide:captions' : 'lucide:video'}
                width="13"
              ></iconify-icon>
            </button>
          {/if}
        </div>

        {#if $capsuleNotice}
          <div class="panel-reminder" class:confirm={$capsuleNotice.kind === 'confirm'}>
            <div class="reminder-head">
              <iconify-icon
                icon={$capsuleNotice.kind === 'confirm' ? 'lucide:bell-ring' : 'lucide:circle-check'}
                width="15"
              ></iconify-icon>
              <span class="reminder-title">
                {$capsuleNotice.kind === 'done' ? $t('capsule.done') : $t('capsule.confirm')}
              </span>
              <span class="reminder-time">{clockLabel}</span>
            </div>
            {#if noticeTitle}<div class="reminder-sub">{noticeTitle}</div>{/if}
            <div class="reminder-hint">{$t('capsule.click_open')}</div>
          </div>
        {:else}
        <div class="panel-cal">
          <div class="cal-head">
            <button type="button" aria-label="previous month" onclick={() => (monthOffset -= 1)}>‹</button>
            <span class="cal-title">{monthDate.getFullYear()} · {monthDate.getMonth() + 1}</span>
            <button type="button" aria-label="next month" onclick={() => (monthOffset += 1)}>›</button>
          </div>
          <div class="cal-grid">
            {#each weekdays as w}
              <span class="cal-w">{w}</span>
            {/each}
            {#each cells as day}
              <span class="cal-d" class:today={isToday(day)}>{day ?? ''}</span>
            {/each}
          </div>
          <div class="cal-foot">{clockLabel}</div>
        </div>
        {/if}
        {#if todayTasks.length > 0}
          <div class="panel-tasks">
            <div class="tasks-head">{$t('capsule.tasks')}</div>
            {#each todayTasks as task (task.name)}
              <button type="button" class="task-row" onclick={() => goto('tasks')}>
                <span class="task-dot"></span>
                <span class="task-name" title={task.name}>{task.name}</span>
                <span class="task-time">{taskTime(task.nextRun)}</span>
              </button>
            {/each}
          </div>
        {/if}
        <div class="panel-actions">
          <button type="button" onclick={() => goto('new')}>
            <iconify-icon icon="lucide:plus" width="13"></iconify-icon>
            <span>{$t('capsule.new_session')}</span>
          </button>
          <button type="button" onclick={() => goto('tasks')}>
            <iconify-icon icon="lucide:calendar-clock" width="13"></iconify-icon>
            <span>{$t('capsule.tasks_short')}</span>
          </button>
          <button type="button" onclick={() => goto('settings')}>
            <iconify-icon icon="lucide:settings" width="13"></iconify-icon>
            <span>{$t('capsule.settings')}</span>
          </button>
        </div>
      </div>
    {/if}
  </div>
{/if}

<style>
  /* Same axis as the window controls in the header row: height 44px, centred
     vertically, fixed to the viewport's top edge. */
  .capsule-wrap {
    position: fixed;
    top: 0;
    left: 50%;
    transform: translateX(-50%);
    height: 44px;
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 45;
    --wails-draggable: no-drag;
  }
  .capsule {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    max-width: min(420px, 60vw);
    height: 26px;
    padding: 0 13px;
    border: 1px solid var(--border);
    border-radius: 999px;
    background: var(--bg-container);
    color: var(--text);
    font-family: inherit;
    font-size: 12.5px;
    line-height: 1;
    cursor: pointer;
    box-shadow: 0 2px 10px rgba(0, 0, 0, 0.08);
    transition: background-color 160ms ease, border-color 160ms ease;
  }
  .capsule:hover { background: var(--bg-table-header); }
  .capsule.alert {
    border-color: var(--c);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--c) 24%, transparent), 0 2px 10px rgba(0, 0, 0, 0.08);
  }
  .capsule-alert-text { color: var(--text); font-weight: 600; }
  .capsule-dot.notice {
    background: #e5484d;
    box-shadow: 0 0 0 3px rgba(229, 72, 77, 0.22);
    animation: cap-pulse 1.2s ease-in-out infinite;
  }
  .panel-reminder {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 14px 12px;
    border-radius: 12px;
    background: color-mix(in srgb, var(--c) 10%, transparent);
    color: var(--c);
  }
  .panel-reminder.confirm {
    background: rgba(229, 72, 77, 0.10);
    color: #e5484d;
  }
  .reminder-head {
    display: flex;
    align-items: center;
    gap: 7px;
  }
  .reminder-title { font-size: 13.5px; font-weight: 700; }
  .reminder-time {
    margin-left: auto;
    font-size: 11px;
    color: var(--text-tertiary);
    font-variant-numeric: tabular-nums;
  }
  .reminder-sub {
    font-size: 12px;
    color: var(--text);
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
  }
  .reminder-hint { font-size: 11px; color: var(--text-tertiary); }
  .capsule-dot {
    width: 7px;
    height: 7px;
    flex: 0 0 auto;
    border-radius: 50%;
    background: var(--c);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--c) 22%, transparent);
  }
  .capsule-dot.beat { animation: cap-pulse 1.6s ease-in-out infinite; }
  @keyframes cap-pulse {
    0%, 100% { transform: scale(1); }
    50% { transform: scale(1.35); }
  }
  .capsule-label {
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
  }
  .capsule-chev {
    flex: 0 0 auto;
    font-size: 10px;
    color: var(--text-tertiary);
  }
  .capsule-panel {
    position: absolute;
    top: 44px;
    left: 50%;
    transform: translateX(-50%);
    width: min(380px, 88vw);
    padding: 12px 14px 12px;
    border: 1px solid var(--border);
    border-radius: 16px;
    background: var(--bg-container);
    box-shadow: 0 14px 40px rgba(0, 0, 0, 0.16);
    display: flex;
    flex-direction: column;
    gap: 10px;
    animation: cap-in 220ms cubic-bezier(0.22, 0.8, 0.3, 1) both;
  }
  @keyframes cap-in {
    from { opacity: 0; transform: translate(-50%, -6px) scale(0.98); }
  }
  .panel-song {
    display: flex;
    align-items: center;
    gap: 9px;
    min-width: 0;
  }
  .song-cover {
    width: 30px;
    height: 30px;
    flex: 0 0 auto;
    border-radius: 8px;
    overflow: hidden;
    display: grid;
    place-items: center;
    background: var(--bg-table-header);
    color: var(--text-tertiary);
  }
  .song-cover img { width: 100%; height: 100%; object-fit: cover; display: block; }
  .song-meta {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    flex: 1;
  }
  .song-title {
    font-size: 12.5px;
    font-weight: 600;
    color: var(--text);
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
  }
  .song-artist {
    font-size: 11px;
    color: var(--text-tertiary);
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
  }
  .song-controls {
    display: flex;
    align-items: center;
    gap: 2px;
    flex: 0 0 auto;
  }
  .song-controls button {
    width: 24px;
    height: 24px;
    border: none;
    border-radius: 6px;
    background: transparent;
    color: var(--text-secondary);
    display: grid;
    place-items: center;
    cursor: pointer;
  }
  .song-controls button:hover { background: var(--hover-neutral); color: var(--text); }
  .song-controls .song-play { color: var(--text); }
  .panel-seek {
    display: flex;
    align-items: center;
    gap: 7px;
    font-size: 10.5px;
    color: var(--text-tertiary);
    font-variant-numeric: tabular-nums;
  }
  .panel-seek input {
    flex: 1;
    min-width: 0;
    accent-color: var(--c);
    height: 14px;
  }
  .panel-lyrics {
    position: relative;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 3px;
    text-align: center;
    color: var(--c);
    min-height: 140px;
    justify-content: center;
    border-top: 1px solid var(--border-secondary);
    border-bottom: 1px solid var(--border-secondary);
    padding: 8px 0;
  }
  .panel-lyrics.video {
    min-height: 0;
    gap: 0;
    padding: 0;
  }
  .panel-video {
    width: 100%;
    max-height: 220px;
    aspect-ratio: 16 / 9;
    object-fit: contain;
    display: block;
    border-radius: 9px;
    background: #000;
  }
  .video-error {
    position: absolute;
    left: 8px;
    bottom: 8px;
    padding: 3px 7px;
    border-radius: 6px;
    background: rgba(229, 72, 77, 0.92);
    color: #fff;
    font-size: 10.5px;
  }
  .lyrics-video-btn {
    position: absolute;
    right: 2px;
    bottom: 2px;
    width: 24px;
    height: 24px;
    display: grid;
    place-items: center;
    border: 1px solid var(--border);
    border-radius: 7px;
    background: var(--bg-container);
    color: var(--text-secondary);
    cursor: pointer;
    z-index: 1;
  }
  .lyrics-video-btn:hover { color: var(--text); background: var(--bg-table-header); }
  .lyrics-video-btn.on { color: var(--c); border-color: var(--c); }
  .pl {
    max-width: 100%;
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
    font-weight: 600;
    line-height: 1.35;
  }
  .pl[data-d='0'] { font-size: 17px; opacity: 0.95; }
  .pl[data-d='-1'], .pl[data-d='1'] { font-size: 13.5px; opacity: 0.55; }
  .pl[data-d='-2'], .pl[data-d='2'] { font-size: 11.5px; opacity: 0.32; }
  .pl[data-d='-3'], .pl[data-d='3'] { font-size: 10.5px; opacity: 0.18; }
  .pl-empty { color: var(--c); opacity: 0.7; font-size: 16px; }
  .panel-cal { padding-top: 2px; }
  .cal-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 6px;
  }
  .cal-head button {
    width: 22px;
    height: 22px;
    border: none;
    border-radius: 6px;
    background: transparent;
    color: var(--text-secondary);
    font-size: 14px;
    cursor: pointer;
  }
  .cal-head button:hover { background: var(--hover-neutral); color: var(--text); }
  .cal-title { font-size: 12.5px; color: var(--text); font-variant-numeric: tabular-nums; }
  .cal-grid {
    display: grid;
    grid-template-columns: repeat(7, 1fr);
    gap: 2px;
    text-align: center;
  }
  .cal-w {
    font-size: 10.5px;
    color: var(--text-tertiary);
    padding: 2px 0;
  }
  .cal-d {
    font-size: 11.5px;
    color: var(--text-secondary);
    padding: 4px 0;
    border-radius: 6px;
    font-variant-numeric: tabular-nums;
  }
  .cal-d.today {
    color: #fff;
    background: var(--c);
    font-weight: 600;
  }
  .cal-foot {
    margin-top: 6px;
    text-align: right;
    font-size: 10.5px;
    color: var(--text-tertiary);
    font-variant-numeric: tabular-nums;
  }
  .panel-tasks {
    display: flex;
    flex-direction: column;
    gap: 3px;
    border-top: 1px solid var(--border-secondary);
    padding-top: 8px;
  }
  .tasks-head { font-size: 11px; color: var(--text-tertiary); margin-bottom: 2px; }
  .task-row {
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 5px 6px;
    border: none;
    border-radius: 7px;
    background: transparent;
    color: var(--text);
    font-family: inherit;
    font-size: 11.5px;
    cursor: pointer;
    text-align: left;
  }
  .task-row:hover { background: var(--hover-neutral); }
  .task-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--c); flex: 0 0 auto; }
  .task-name { flex: 1; min-width: 0; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
  .task-time { color: var(--text-tertiary); font-variant-numeric: tabular-nums; }
  .panel-actions {
    display: flex;
    gap: 6px;
    border-top: 1px solid var(--border-secondary);
    padding-top: 8px;
  }
  .panel-actions button {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 5px;
    padding: 6px 4px;
    border: 1px solid var(--border);
    border-radius: 8px;
    background: transparent;
    color: var(--text-secondary);
    font-family: inherit;
    font-size: 11.5px;
    cursor: pointer;
  }
  .panel-actions button:hover { background: var(--hover-neutral); color: var(--text); }
  @media (prefers-reduced-motion: reduce) {
    .capsule-panel { animation: none; }
    .capsule { transition: none; }
    .capsule-dot.beat { animation: none; }
  }
</style>
