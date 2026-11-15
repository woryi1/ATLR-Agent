// Music player state + the one <audio> element.
//
// The browser cannot play Bilibili audio directly (the CDN wants a Referer),
// so the source is the local proxy in internal/music. Everything the user cares
// about — which track, playing or paused, and where in the track — is restored
// on launch from /api/music/state, and saved back while it changes.
import { get, writable } from 'svelte/store'
import { nativeShell } from './stores'
import * as api from './api'

export interface Track {
  bvid: string
  title: string
  artist: string
  cover: string
  duration: number
}

export const musicTracks = writable<Track[]>([])
export const musicIndex = writable(0)
export const musicPlaying = writable(false)
export const musicPosition = writable(0)
export const musicDuration = writable(0)
export const musicReady = writable(false)
export const musicLyrics = writable<{ from: number; to: number; text: string }[]>([])
/** The track actually loaded in the element — playlist or a search result. */
export const musicNow = writable<Track | null>(null)

// ── Visualiser ───────────────────────────────────────────────────────────────
// Settings live in one object so the settings pane, the sidebar shortcut and the
// component itself can never disagree. Defaults: on, 88px wide, 0.8 opacity,
// theme accent (white is opt-in).
export interface VizSettings {
  enabled: boolean
  width: number
  opacity: number
  white: boolean
}

const VIZ_DEFAULTS: VizSettings = { enabled: true, width: 88, opacity: 0.8, white: false }

function loadVizSettings(): VizSettings {
  try {
    const raw = localStorage.getItem('atlr.viz')
    if (!raw) return { ...VIZ_DEFAULTS }
    const parsed = JSON.parse(raw) as Partial<VizSettings>
    return {
      enabled: typeof parsed.enabled === 'boolean' ? parsed.enabled : VIZ_DEFAULTS.enabled,
      width: Number(parsed.width) >= 32 ? Number(parsed.width) : VIZ_DEFAULTS.width,
      opacity: Number(parsed.opacity) > 0 ? Math.min(1, Number(parsed.opacity)) : VIZ_DEFAULTS.opacity,
      white: parsed.white === true,
    }
  } catch {
    return { ...VIZ_DEFAULTS }
  }
}

export const vizSettings = writable<VizSettings>(loadVizSettings())

export function patchVizSettings(patch: Partial<VizSettings>): void {
  vizSettings.update((current) => {
    const next = { ...current, ...patch }
    try { localStorage.setItem('atlr.viz', JSON.stringify(next)) } catch { /* ignore */ }
    return next
  })
}

export function setVizColorWhite(on: boolean): void {
  patchVizSettings({ white: on })
}

const VIZ_BANDS = 22
const VIZ_LOW_HZ = 40
const VIZ_HIGH_HZ = 16000
let vizCtx: AudioContext | null = null
let vizAnalyserL: AnalyserNode | null = null
let vizAnalyserR: AnalyserNode | null = null
let vizSampleRate = 48000
let vizBins = 512
const vizFreqL = new Uint8Array(512)
const vizFreqR = new Uint8Array(512)

// Built lazily once per audio element: a MediaElementSource may only be created
// once, and the context cannot leave "suspended" without a user gesture — the
// play listener resumes it, which is why the graph is created up front.
function ensureVizGraph(): void {
  if (vizCtx || !audio) return
  const Ctor = window.AudioContext ??
    (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
  if (!Ctor) return
  try {
    const ctx = new Ctor()
    const src = ctx.createMediaElementSource(audio)
    const split = ctx.createChannelSplitter(2)
    src.connect(split)
    const l = ctx.createAnalyser()
    const r = ctx.createAnalyser()
    for (const a of [l, r]) {
      a.fftSize = 1024
      // Light smoothing on top of the component's own attack/decay: enough to
      // stop frame-to-frame noise, not enough to flatten the beat.
      a.smoothingTimeConstant = 0.45
      // A music stream sits well below a full-scale reference; widening the dB
      // window is what keeps quiet passages from reading as an empty bar.
      a.minDecibels = -90
      a.maxDecibels = -20
    }
    split.connect(l, 0)
    split.connect(r, 1)
    // Both analysers feed the speakers; without this the source is silent.
    l.connect(ctx.destination)
    r.connect(ctx.destination)
    vizCtx = ctx
    vizAnalyserL = l
    vizAnalyserR = r
    vizSampleRate = ctx.sampleRate
    vizBins = l.frequencyBinCount
  } catch {
    vizCtx = null
  }
}

/**
 * Read both channels as 22 log-spaced bands (0–255). Each band takes the peak
 * bin rather than the mean, so a kick or a bass line actually kicks; a mono
 * track leaves the right channel at zero, which is what makes only the left
 * column move.
 */
export function readVizBands(): { left: number[]; right: number[]; live: boolean } {
  const live = !!(audio && !audio.paused && vizAnalyserL && vizAnalyserR)
  if (live && vizAnalyserL && vizAnalyserR) {
    if (vizCtx?.state === 'suspended') void vizCtx.resume()
    vizAnalyserL.getByteFrequencyData(vizFreqL)
    vizAnalyserR.getByteFrequencyData(vizFreqR)
  }
  const left: number[] = []
  const right: number[] = []
  const binHz = vizSampleRate / 1024
  const ratio = VIZ_HIGH_HZ / VIZ_LOW_HZ
  for (let i = 0; i < VIZ_BANDS; i++) {
    const fromHz = VIZ_LOW_HZ * Math.pow(ratio, i / VIZ_BANDS)
    const toHz = VIZ_LOW_HZ * Math.pow(ratio, (i + 1) / VIZ_BANDS)
    const from = Math.max(0, Math.min(vizBins - 1, Math.round(fromHz / binHz)))
    const to = Math.max(from + 1, Math.min(vizBins, Math.round(toHz / binHz)))
    let peakL = 0
    let peakR = 0
    for (let b = from; b < to; b++) {
      if (vizFreqL[b] > peakL) peakL = vizFreqL[b]
      if (vizFreqR[b] > peakR) peakR = vizFreqR[b]
    }
    // 0.75 curve lifts the low end of the scale without hiding the peak.
    left.push(live ? Math.round(Math.pow(peakL / 255, 0.75) * 255) : 0)
    right.push(live ? Math.round(Math.pow(peakR / 255, 0.75) * 255) : 0)
  }
  return { left, right, live }
}

// ── Library panel state ──────────────────────────────────────────────────────
// Lives here rather than inside the sidebar so closing the panel (or the
// sidebar remounting) does not throw away the keyword and the results: the
// next open shows the same search instead of dropping back to the favourites.
export const musicLibOpen = writable(false)
export const musicLibSearchOpen = writable(false)
export const musicLibQuery = writable('')
export const musicLibRemote = writable<Track[]>([])
export const musicLibSearching = writable(false)
/** Videos favourited during this session (bvid), for the filled star. */
export const musicFaved = writable<Set<string>>(new Set())

let libSearchTimer: ReturnType<typeof setTimeout> | null = null
let libSearchSeq = 0

/** Set the keyword and (debounced) run the folder + Bilibili search. */
export function setMusicLibQuery(value: string): void {
  musicLibQuery.set(value)
  const term = value.trim()
  const seq = ++libSearchSeq
  if (libSearchTimer) {
    clearTimeout(libSearchTimer)
    libSearchTimer = null
  }
  if (!term) {
    musicLibRemote.set([])
    musicLibSearching.set(false)
    return
  }
  musicLibSearching.set(true)
  libSearchTimer = setTimeout(async () => {
    const res = await searchMusicRemote(term)
    if (seq !== libSearchSeq) return
    musicLibRemote.set(res)
    musicLibSearching.set(false)
  }, 350)
}

/** Add a video to the account's favourites; returns the folder it landed in. */
export async function favoriteTrack(t: Track): Promise<{ ok: boolean; folder: string; message?: string }> {
  try {
    const res = await fetch('/api/music/favorite', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ bvid: t.bvid }),
    })
    const body = await res.json().catch(() => null)
    if (!res.ok || !body?.ok) {
      return { ok: false, folder: '', message: body?.error || '收藏失败' }
    }
    musicFaved.update(s => new Set(s).add(t.bvid))
    return { ok: true, folder: body.folder ?? '' }
  } catch {
    return { ok: false, folder: '', message: '网络异常' }
  }
}

/** Bilibili images (covers, avatars) 403 without a Referer: proxy them locally. */
export function proxyImg(u: string): string {
  return u ? `/api/bili/img?url=${encodeURIComponent(u)}` : ''
}

/** Load the timed lyrics for a video; empty means "no subtitles". */
let lyricsToken = 0
let lyricsFor = ''
export async function loadLyrics(bvid: string): Promise<void> {
  const token = ++lyricsToken
  if (!bvid) {
    lyricsFor = ''
    musicLyrics.set([])
    return
  }
  // A different track never keeps the previous one's lines around: that is how
  // a track without subtitles still showed a lyric button (and the wrong words).
  if (bvid !== lyricsFor) {
    lyricsFor = bvid
    musicLyrics.set([])
  }
  try {
    const res = await fetch(`/api/music/lyrics?bvid=${encodeURIComponent(bvid)}`)
    if (token !== lyricsToken) return // a newer track already asked
    if (!res.ok) {
      musicLyrics.set([])
      return
    }
    const body = await res.json().catch(() => null)
    if (token !== lyricsToken) return
    // The host echoes the bvid it answered for; a mismatched reply is dropped.
    if (body?.bvid && body.bvid !== bvid) return
    const lines = (body?.lines ?? []).slice(0, 5000)
    musicLyrics.set(Array.isArray(lines) ? lines : [])
  } catch {
    if (token === lyricsToken) musicLyrics.set([])
  }
}

/** Open the track's video page (system browser or a new web window). */
export async function openMusicVideo(bvid: string): Promise<void> {
  try {
    const res = await fetch('/api/music/open', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ bvid }),
    })
    if (!res.ok) return
    const body = await res.json().catch(() => null)
    // The server answers either {opened:true} (system browser) or {url} (in-app).
    // In the desktop shell the page cannot window.open, so the URL goes back
    // through the native bridge and the shell creates the window.
    if (body?.url) {
      if (get(nativeShell)) await api.openWebWindow(body.url)
      else window.open(body.url, '_blank', 'width=1100,height=760')
    }
  } catch { /* ignore */ }
}

/** Force a playlist refetch (used after login and by the panel's retry). */
export async function forceRefreshPlaylist(): Promise<void> {
  await retryMusicPlaylist(true)
}

/** Fetch in the background and hold the result for the next panel open. */
export async function stagePlaylistRefresh(): Promise<void> {
  try {
    const res = await fetch('/api/music/playlist')
    if (!res.ok) return
    const body = await res.json().catch(() => null)
    const tracks: Track[] = body?.tracks ?? []
    if (tracks.length) pendingTracks = tracks
  } catch { /* keep the current list */ }
}

/** Swap in the staged list (next open). Keeps the playing track by bvid. */
export function applyPendingPlaylist(): void {
  const fresh = pendingTracks
  pendingTracks = null
  if (!fresh || !fresh.length) return
  const prev = get(musicTracks)
  if (prev.length > 0 && fresh.length < prev.length) return
  const playing = prev[get(musicIndex)]?.bvid ?? ''
  const prevIdx = get(musicIndex)
  musicTracks.set(fresh)
  const idx = playing ? Math.max(0, fresh.findIndex(t => t.bvid === playing)) : Math.min(prevIdx, fresh.length - 1)
  musicIndex.set(idx)
  if (!get(musicNow) || fresh.some(t => t.bvid === get(musicNow)!.bvid)) {
    musicNow.set(fresh[idx])
  }
  if (audio && (idx !== prevIdx || !playing)) audio.src = srcFor(idx)
}

/** True when the last session was playing; try to pick it back up after boot. */
export function resumeMusicIfWanted(): void {
  if (!wantResume || !audio || !audio.paused) return
  wantResume = false
  void audio.play().catch(() => { /* still no gesture: stays paused */ })
}

let audio: HTMLAudioElement | null = null
let saveTimer: ReturnType<typeof setTimeout> | null = null
let initialized = false
let wantResume = false
// A refreshed library waits here until the NEXT panel open, so the visible
// list never jumps under the user's finger.
let pendingTracks: Track[] | null = null

function trackAt(i: number): Track | null {
  return get(musicTracks)[i] ?? null
}

function srcFor(i: number): string {
  const t = trackAt(i)
  return t ? `/api/music/stream/${encodeURIComponent(t.bvid)}` : ''
}

function save(immediate = false): void {
  if (saveTimer) {
    clearTimeout(saveTimer)
    saveTimer = null
  }
  const fire = () => {
    void fetch('/api/music/state', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        index: get(musicIndex),
        paused: !get(musicPlaying),
        position: get(musicPosition),
      }),
    }).catch(() => {})
  }
  if (immediate) fire()
  else saveTimer = setTimeout(fire, 2000)
}

function attach(a: HTMLAudioElement, restore: number): void {
  a.preload = 'auto'
  a.addEventListener('timeupdate', () => { musicPosition.set(a.currentTime); save() })
  a.addEventListener('durationchange', () => { musicDuration.set(Number.isFinite(a.duration) ? a.duration : 0) })
  a.addEventListener('play', () => { musicPlaying.set(true); save(true) })
  a.addEventListener('pause', () => { musicPlaying.set(false); save(true) })
  a.addEventListener('ended', () => nextMusic())
  a.addEventListener('loadedmetadata', () => {
    if (restore > 0) {
      try { a.currentTime = restore } catch { /* seek before data is fine to skip */ }
    }
  }, { once: true })
}

/** Load the playlist + saved state and prepare the audio element. Idempotent. */
export async function initMusic(): Promise<void> {
  if (initialized || typeof window === 'undefined') return
  initialized = true
  try {
    const [plRes, stateRes] = await Promise.all([
      fetch('/api/music/playlist').catch(() => null),
      fetch('/api/music/state').catch(() => null),
    ])
    if (!plRes || !plRes.ok) return
    const body = await plRes.json().catch(() => null)
    const tracks: Track[] = body?.tracks ?? []
    if (!tracks.length) return
    const st = stateRes && stateRes.ok ? await stateRes.json().catch(() => null) : null

    musicTracks.set(tracks)
    const idx = Math.min(Math.max(0, Number(st?.index) || 0), tracks.length - 1)
    musicIndex.set(idx)
    musicNow.set(tracks[idx])

    audio = new Audio()
    attach(audio, Number(st?.position) || 0)
    audio.src = srcFor(idx)
    ensureVizGraph()
    audio.addEventListener('play', () => {
      if (vizCtx?.state === 'suspended') void vizCtx.resume()
    })
    musicReady.set(true)

    // Never start audio here: the sidebar mounts behind the boot splash, and a
    // song under the opening animation is exactly the bug this flag avoids.
    // resumeMusicIfWanted() runs once the splash hands over.
    if (st?.paused === false) wantResume = true
  } catch { /* no playlist (offline): the button stays "休息" */ }
}

/** Fill an empty playlist (panel open). `force` bypasses the host snapshot. */
export async function retryMusicPlaylist(force = false): Promise<void> {
  if (!force && get(musicTracks).length > 0) return
  try {
    const res = await fetch(force ? '/api/music/playlist?refresh=1' : '/api/music/playlist')
    if (!res.ok) return
    const body = await res.json().catch(() => null)
    const tracks: Track[] = body?.tracks ?? []
    if (!tracks.length) return
    const prev = get(musicTracks)
    // A partial answer must never shrink the library nor restart the song.
    if (prev.length > 0 && tracks.length < prev.length) return
    const playing = prev[get(musicIndex)]?.bvid ?? ''
    const prevIdx = get(musicIndex)
    musicTracks.set(tracks)
    const idx = playing
      ? Math.max(0, tracks.findIndex(t => t.bvid === playing))
      : Math.min(prevIdx, tracks.length - 1)
    musicIndex.set(idx)
    if (!get(musicNow) || tracks.some(t => t.bvid === get(musicNow)!.bvid)) {
      musicNow.set(tracks[idx])
    }
    if (!audio) {
      await initMusic()
    } else if (idx !== prevIdx || !playing) {
      // Only re-point the element when the current track actually changed.
      audio.src = srcFor(idx)
    }
    musicReady.set(true)
  } catch { /* still blocked: the button keeps saying 休息 */ }
}

export function toggleMusic(): void {
  if (!audio) return
  if (audio.paused) void audio.play().catch(() => {})
  else audio.pause()
}

export function selectTrack(i: number, play: boolean): void {
  const tracks = get(musicTracks)
  if (!tracks.length) return
  const idx = ((i % tracks.length) + tracks.length) % tracks.length
  musicIndex.set(idx)
  musicNow.set(tracks[idx])
  musicPosition.set(0)
  if (!audio) return
  audio.src = srcFor(idx)
  if (play) void audio.play().catch(() => {})
  save(true)
}

/** Play an arbitrary track (a Bilibili search result, not in the two folders). */
export function playTrack(t: Track): void {
  musicNow.set(t)
  musicPosition.set(0)
  const idx = get(musicTracks).findIndex(x => x.bvid === t.bvid)
  if (idx >= 0) musicIndex.set(idx)
  if (!audio) return
  audio.src = `/api/music/stream/${encodeURIComponent(t.bvid)}`
  void audio.play().catch(() => {})
  save(true)
}

/** Bilibili-wide search results, appended after the local folder matches. */
export async function searchMusicRemote(q: string): Promise<Track[]> {
  const term = q.trim()
  if (!term) return []
  try {
    const res = await fetch(`/api/music/search?q=${encodeURIComponent(term)}`)
    if (!res.ok) return []
    const body = await res.json().catch(() => null)
    return body?.tracks ?? []
  } catch {
    return []
  }
}

export function nextMusic(): void {
  selectTrack(get(musicIndex) + 1, true)
}

export function prevMusic(): void {
  selectTrack(get(musicIndex) - 1, true)
}

export function seekMusic(seconds: number): void {
  if (!audio) return
  try { audio.currentTime = seconds } catch { /* ignore */ }
  musicPosition.set(seconds)
  save()
}
