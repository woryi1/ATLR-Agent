// Theme system — two orthogonal axes applied as <html> attributes:
//   data-theme       resolved mode: "light" | "dark"
//   data-theme-pack  palette family: "azure" (default) | one of PACKS below
//                    | the id of a user theme under ~/.octo/themes/
//
// The user's *choice* of mode is "light" | "dark" | "system"; "system" tracks
// the OS preference live. The two axes are independent: every pack ships both
// a light and a dark palette, so switching pack never changes mode.

import { writable } from 'svelte/store'
import { getThemeSelection, listThemes, saveThemeSelection } from './api'
import { storageKey } from './storageKey'

export type ThemeMode = 'light' | 'dark' | 'system'

// Wallpaper choices: '' / 'off' render the plain app; 'bloom', 'mystery' and
// 'meadow' are the wallpapers baked into the binary; 'custom' is the user's own
// upload (served from Data/backgrounds/).
export type BackgroundID = '' | 'off' | 'bloom' | 'mystery' | 'meadow' | 'custom'
// Animated wallpapers baked into the binary. Only one ships today; the id is
// sent to the server as-is and must be in internal/server's
// backgroundVideoAssets map.
export type BackgroundVideoID = '' | 'sakura'
export type BackgroundFit = 'cover' | 'contain' | 'fill'

const MODE_KEY = storageKey('themeMode')
const PACK_KEY = storageKey('themePack')
const BG_KEY = storageKey('background')
const BG_VIDEO_KEY = storageKey('bgVideo')
const BG_VIDEO_SOUND_KEY = storageKey('bgVideoSound')
const BG_FIT_KEY = storageKey('bgFit')
const BG_OPACITY_KEY = storageKey('bgOpacity')
const BG_BLUR_KEY = storageKey('bgBlur')
const ANIM_KEY = storageKey('startupAnim')
const DEFAULT_PACK = 'azure'

// The first-run experience this rebuild ships: the 少女 (blossom) palette in
// dark mode with the first built-in wallpaper. Only used while the server has
// no persisted selection — after that, the user's own choice wins.
const FIRST_RUN_MODE: ThemeMode = 'dark'
const FIRST_RUN_PACK = 'blossom'
const FIRST_RUN_BACKGROUND: BackgroundID = 'bloom'

// Ids this app has shipped under a different name. A stored value listed here
// is read as its current id, so a user keeps the pack they chose across a
// rename. Renaming the *default* pack would survive without this — an unknown
// id already falls back to the default — but relying on that only works for
// that one pack, and silently stops working the day a pack reuses the old id.
// celestia was retired in favour of ocean, which now ships as a seed theme
// (internal/server/themes). Mapping it keeps anyone who was using it on a
// theme rather than dropping them back to the default.
const RENAMED_PACKS: Record<string, string> = { klook: 'azure', celestia: 'ocean' }

// The one pack compiled into the app: something has to render before any
// theme file is read, and that is the default. Every other theme octo ships
// (ocean, blossom, vogue) is seeded to ~/.octo/themes/ on first run and
// arrives here through loadUserPacks like a theme anyone else wrote — which
// is the point: they are editable, deletable, and worked examples.
//
// `swatch` is the pair the picker draws — accent over surface, in the pack's
// *light* palette — so the control can show what it looks like without
// applying it. `labelKey` is the i18n key for the display name.
export type ThemePack = {
  id: string
  labelKey: string
  swatch: [accent: string, surface: string]
}

export const PACKS: ThemePack[] = [
  { id: 'azure', labelKey: 'settings.pack_azure', swatch: ['#007AFF', '#F5F5F7'] },
]

const PACK_IDS = new Set(PACKS.map((p) => p.id))

let systemMql: MediaQueryList | null = null
let systemListener: ((e: MediaQueryListEvent) => void) | null = null

// ─── User theme packs ───────────────────────────────────────────────────────
//
// A theme under ~/.octo/themes/<id>/ is a manifest plus a stylesheet that
// redefines the same variables app.css declares, scoped to its own
// `[data-theme-pack="<id>"]`. The server lists and serves them; this module
// links the stylesheet and lets the id pass normalizePack.

// What the picker renders: the built-in packs name their label through i18n, a
// user theme carries the name its manifest gave. Kept separate from ThemePack
// so the built-in list stays a compile-time constant with a required labelKey.
export type PackChoice = {
  id: string
  labelKey?: string
  label?: string
  // Per-locale names from the manifest; the picker prefers the current
  // locale's and falls back to `label`. This is how the seeded themes keep
  // the Chinese names they had as i18n keys (少女, 时尚, 深海).
  labels?: Record<string, string>
  swatch: [accent: string, surface: string]
  author?: string
  homepage?: string
}

// A user theme without a swatch still needs a chip. Current-colour over the
// container surface reads as "no preview", which is honest, and both values are
// ours rather than the manifest's.
const NEUTRAL_SWATCH: [string, string] = ['var(--text-tertiary)', 'var(--bg-container)']

const builtinChoices = (): PackChoice[] =>
  PACKS.map((p) => ({ id: p.id, labelKey: p.labelKey, swatch: p.swatch }))

// Ids discovered at runtime. Empty until loadUserPacks resolves — see initTheme
// for why boot does not wait on it.
let userPackIDs = new Set<string>()

// The picker's list. Replaced wholesale once user themes arrive.
export const packs = writable<PackChoice[]>(builtinChoices())

// Current choice. Boot seeds it from localStorage (fast, no flash); once the
// server answers, Data/themes/selection.json is the source of truth and the
// localStorage copy is only an offline fallback. See loadThemeSelection.
let currentMode: ThemeMode = 'light'
let currentPack = DEFAULT_PACK
let currentBackground: BackgroundID = ''
let currentBgVideo: BackgroundVideoID = ''
let currentBgVideoSound = false
let currentBgFit: BackgroundFit = 'cover'
let currentBgOpacity = 0.2
let currentBgBlur = 0
let currentStartupAnim = true

export function getMode(): ThemeMode {
  return currentMode
}

// A pack id that is no longer shipped (a removed pack, a hand-edited value)
// falls back to the default rather than writing an attribute no CSS matches,
// which would otherwise leave the app on the default palette with the picker
// showing nothing selected. A user theme counts as shipped once its stylesheet
// is linked, and stops counting the moment its directory goes away.
function normalizePack(id: string | null): string {
  if (!id) return DEFAULT_PACK
  const current = RENAMED_PACKS[id] ?? id
  return PACK_IDS.has(current) || userPackIDs.has(current) ? current : DEFAULT_PACK
}

export function getPack(): string {
  // Reads the stored choice live rather than trusting the boot seed. The seed
  // normalized a stored user-theme id to the default — its stylesheet does not
  // exist until loadUserPacks links it — and the localStorage copy is the
  // survival contract every writer (setPack, loadThemeSelection) keeps in
  // sync, so it is the one place the answer can always come from.
  return normalizePack(localStorage.getItem(PACK_KEY))
}

function prefersDark(): boolean {
  return typeof matchMedia !== 'undefined' && matchMedia('(prefers-color-scheme: dark)').matches
}

function resolveMode(mode: ThemeMode): 'light' | 'dark' {
  if (mode === 'system') return prefersDark() ? 'dark' : 'light'
  return mode
}

// apply writes the attributes and, for "system", installs a listener so the
// app follows the OS theme as it changes.
function apply(mode: ThemeMode, pack: string): void {
  const root = document.documentElement
  root.setAttribute('data-theme', resolveMode(mode))
  // The default pack lives in :root, so only set the attribute for others.
  if (pack && pack !== DEFAULT_PACK) root.setAttribute('data-theme-pack', pack)
  else root.removeAttribute('data-theme-pack')

  if (systemMql && systemListener) {
    systemMql.removeEventListener('change', systemListener)
    systemMql = null
    systemListener = null
  }
  if (mode === 'system' && typeof matchMedia !== 'undefined') {
    systemMql = matchMedia('(prefers-color-scheme: dark)')
    systemListener = () => {
      document.documentElement.setAttribute('data-theme', prefersDark() ? 'dark' : 'light')
    }
    systemMql.addEventListener('change', systemListener)
  }
}

export function setMode(mode: ThemeMode): void {
  currentMode = mode
  localStorage.setItem(MODE_KEY, mode)
  apply(mode, currentPack)
  void persistSelection()
}

export function getBackground(): BackgroundID {
  return currentBackground
}

export function getBackgroundVideo(): BackgroundVideoID {
  return currentBgVideo
}

export function getBackgroundVideoSound(): boolean {
  return currentBgVideoSound
}

export function getBackgroundAppearance(): { fit: BackgroundFit; opacity: number; blur: number } {
  return { fit: currentBgFit, opacity: currentBgOpacity, blur: currentBgBlur }
}

// setBackgroundAppearance adjusts how the wallpaper renders (fit / opacity /
// blur) and mirrors the values to the server. Only meaningful while a
// background is actually set.
export function setBackgroundAppearance(fit: BackgroundFit, opacity: number, blur: number): void {
  currentBgFit = fit
  currentBgOpacity = Math.min(0.8, Math.max(0.05, opacity))
  currentBgBlur = Math.min(30, Math.max(0, blur))
  localStorage.setItem(BG_FIT_KEY, currentBgFit)
  localStorage.setItem(BG_OPACITY_KEY, String(currentBgOpacity))
  localStorage.setItem(BG_BLUR_KEY, String(currentBgBlur))
  applyBackgroundLayer()
  void persistSelection()
}

// getStartupAnim/setStartupAnim gate the boot splash. The localStorage copy is
// what the index.html inline script reads synchronously before anything else
// can run; the server copy makes the choice migrate with the Data directory.
export function getStartupAnim(): boolean {
  return currentStartupAnim
}

export function setStartupAnim(v: boolean): void {
  currentStartupAnim = v
  localStorage.setItem(ANIM_KEY, v ? '1' : '0')
  void persistSelection()
}

// setBackground switches the wallpaper and mirrors the choice to the server.
// 'off' (or '') removes the layer entirely — the plain octo look. Picking a
// still image also retires the animated one: the server enforces the same
// exclusivity, and leaving the local video id set would disagree with it.
export function setBackground(bg: BackgroundID): void {
  currentBackground = bg
  currentBgVideo = ''
  localStorage.removeItem(BG_VIDEO_KEY)
  if (bg && bg !== 'off') localStorage.setItem(BG_KEY, bg)
  else localStorage.removeItem(BG_KEY)
  applyBackgroundLayer()
  void persistSelection()
}

// setBackgroundVideo plays one of the built-in animated wallpapers. Same
// exclusivity as setBackground, from the other side: the still wallpaper is
// cleared, so exactly one layer is ever mounted.
export function setBackgroundVideo(id: BackgroundVideoID): void {
  currentBgVideo = id
  if (id) {
    currentBackground = ''
    localStorage.removeItem(BG_KEY)
    localStorage.setItem(BG_VIDEO_KEY, id)
  } else {
    localStorage.removeItem(BG_VIDEO_KEY)
  }
  applyBackgroundLayer()
  void persistSelection()
}

// setBackgroundVideoSound only toggles the audio track of the layer already
// on screen — the video keeps its position, so this does not go through
// applyBackgroundLayer (which would reassign src and restart playback).
export function setBackgroundVideoSound(on: boolean): void {
  currentBgVideoSound = on
  localStorage.setItem(BG_VIDEO_SOUND_KEY, on ? '1' : '0')
  const el = activeEl ?? document.getElementById('atlr-bg')
  if (el instanceof HTMLVideoElement) {
    el.muted = !on
    if (on) el.play().catch(() => {})
  }
  void persistSelection()
}

// Wallpaper layer lifecycle.
//
// Exactly one layer — a still image or an animated video — may be mounted, and
// the boot animation is the third owner of the screen. A switch is progressive:
// the incoming layer is built and starts loading while the outgoing one is
// still visible; only once the new layer has painted (first decoded image /
// video frame, or a bounded timeout) is the old one torn down. That ordering is
// what keeps a switch from flashing the bare app.
//
// A guard re-checks the mounted layer every 10 seconds: a video that dropped
// out is restarted, a node that vanished is rebuilt, and a layer that should no
// longer exist is removed. Rebuilds are rate-limited so a broken source cannot
// spin the page.
let activeEl: HTMLImageElement | HTMLVideoElement | null = null
let layerGen = 0
let layerReady: Promise<void> = Promise.resolve()
let resolveLayerReady: (() => void) | null = null
let layerGuardStarted = false
let lastRebuildAt = 0

const LAYER_READY_TIMEOUT_MS = 8000
const LAYER_GUARD_MS = 10_000
const LAYER_REBUILD_COOLDOWN_MS = 10_000

// whenBackgroundReady resolves once the boot-time wallpaper has painted its
// first frame (or given up). main.ts waits on it before dismissing the start-up
// animation, so the handover never shows a bare page.
export function whenBackgroundReady(): Promise<void> {
  return layerReady
}

function beginLayerReady(): Promise<void> {
  layerReady = new Promise<void>((resolve) => { resolveLayerReady = resolve })
  return layerReady
}

function finishLayerReady(): void {
  const resolve = resolveLayerReady
  resolveLayerReady = null
  resolve?.()
}

function wantedLayerKind(): 'image' | 'video' | null {
  if (currentBgVideo) return 'video'
  if (currentBackground && currentBackground !== 'off') return 'image'
  return null
}

function matchesKind(el: HTMLElement, kind: 'image' | 'video'): boolean {
  return kind === 'video' ? el.tagName === 'VIDEO' : el.tagName === 'IMG'
}

// applyBackgroundLayer renders the wallpaper as a fixed, non-interactive
// overlay above the app (a low-opacity watermark keeps every surface readable,
// whatever the theme). Managed here rather than in a Svelte component so it
// exists from boot and survives route changes for free.
function applyBackgroundLayer(): void {
  const kind = wantedLayerKind()
  const imageID = currentBackground
  const videoID = currentBgVideo
  if (!kind) {
    if (activeEl) {
      disposeBackgroundLayer(activeEl)
      activeEl = null
    }
    finishLayerReady()
    return
  }
  // The right kind is already mounted: refresh its appearance (fit / opacity /
  // blur, and video sound) without reloading, which would restart playback.
  if (activeEl && activeEl.isConnected && matchesKind(activeEl, kind)) {
    styleLayer(activeEl, kind, videoID, imageID, currentBgOpacity)
    finishLayerReady()
    startLayerGuard()
    return
  }
  // Build the incoming layer first; retire the outgoing one only after the new
  // one is ready to paint.
  const gen = ++layerGen
  beginLayerReady()
  const incoming = kind === 'video' ? createBackgroundVideo() : createBackgroundImage()
  document.body.appendChild(incoming)
  styleLayer(incoming, kind, videoID, imageID, 0)
  void waitForLayerPaint(incoming, kind).then(() => {
    // A newer switch overtook this one: drop the half-loaded node.
    if (gen !== layerGen) {
      disposeBackgroundLayer(incoming)
      return
    }
    const outgoing = activeEl
    incoming.id = 'atlr-bg'
    incoming.style.transition = 'opacity 240ms ease'
    incoming.style.opacity = String(currentBgOpacity)
    activeEl = incoming
    if (outgoing && outgoing !== incoming) disposeBackgroundLayer(outgoing)
    if (kind === 'video') bindBackgroundVisibility()
    finishLayerReady()
    startLayerGuard()
  })
}

// styleLayer applies the shared appearance controls and points the element at
// its source. Only a real source change is written, so moving a slider does not
// restart the video.
function styleLayer(
  el: HTMLImageElement | HTMLVideoElement,
  kind: 'image' | 'video',
  videoID: BackgroundVideoID,
  imageID: BackgroundID,
  opacity: number,
): void {
  el.style.objectFit = currentBgFit
  el.style.opacity = String(opacity)
  el.style.filter = currentBgBlur > 0 ? `blur(${currentBgBlur}px)` : ''
  if (kind === 'video') {
    const v = el as HTMLVideoElement
    // Never let the wallpaper's audio compete with the boot animation: the
    // layer is already loading underneath the splash, but it stays silent
    // until the splash hands the screen over (see the done listener below).
    const splashUp = !!document.getElementById('dsh-startup-root')
    v.muted = !currentBgVideoSound || splashUp
    if (v.muted) v.setAttribute('muted', '')
    else v.removeAttribute('muted')
    const src = `/api/backgrounds/video/${encodeURIComponent(videoID)}`
    if (v.getAttribute('src') !== src) v.src = src
    // A hidden window keeps decoding a 1080p loop for pixels nobody sees.
    // Pause while hidden and resume on the way back.
    if (document.hidden) v.pause()
    else v.play().catch(() => {})
    return
  }
  const img = el as HTMLImageElement
  // Cache-bust only the custom pick, so a freshly uploaded wallpaper shows
  // immediately; the two built-ins stay on stable, cacheable URLs.
  const stable = `/api/backgrounds/${encodeURIComponent(imageID)}`
  const src = imageID === 'custom' ? `${stable}?v=${Date.now()}` : stable
  if (img.getAttribute('src') !== src) img.src = src
}

// waitForLayerPaint resolves on the first decoded frame (or image), on error,
// or after the timeout — the last case keeps a slow source from blocking the
// handover forever.
function waitForLayerPaint(
  el: HTMLImageElement | HTMLVideoElement,
  kind: 'image' | 'video',
): Promise<void> {
  return new Promise((resolve) => {
    let done = false
    const finish = () => {
      if (done) return
      done = true
      clearTimeout(timer)
      el.removeEventListener('load', finish)
      el.removeEventListener('loadeddata', finish)
      el.removeEventListener('error', finish)
      resolve()
    }
    const timer = setTimeout(finish, LAYER_READY_TIMEOUT_MS)
    if (kind === 'video') {
      el.addEventListener('loadeddata', finish, { once: true })
      if ((el as HTMLVideoElement).readyState >= 2) finish()
    } else {
      el.addEventListener('load', finish, { once: true })
      if ((el as HTMLImageElement).complete) finish()
    }
    el.addEventListener('error', finish, { once: true })
  })
}

// startLayerGuard installs the 10-second self-check once. It restarts a video
// that stopped, rebuilds a layer whose node disappeared, and removes a layer
// the user has since turned off.
function startLayerGuard(): void {
  if (layerGuardStarted || typeof window === 'undefined') return
  layerGuardStarted = true
  window.setInterval(() => {
    if (document.hidden) return
    const kind = wantedLayerKind()
    if (!kind) {
      if (activeEl) {
        disposeBackgroundLayer(activeEl)
        activeEl = null
      }
      return
    }
    if (!activeEl || !activeEl.isConnected || !matchesKind(activeEl, kind)) {
      rebuildLayer()
      return
    }
    if (kind === 'video') {
      const v = activeEl as HTMLVideoElement
      if (v.paused) v.play().catch(() => {})
    }
  }, LAYER_GUARD_MS)
}

// rebuildLayer is the "it dropped out, bring it back" path. Rate-limited so a
// source that keeps failing cannot rebuild in a loop; the guard retries after
// the cooldown.
function rebuildLayer(): void {
  const now = Date.now()
  if (now - lastRebuildAt < LAYER_REBUILD_COOLDOWN_MS) return
  lastRebuildAt = now
  applyBackgroundLayer()
}

// The two sources need different elements, so a switch retires the old node.
function disposeBackgroundLayer(el: HTMLElement | null): void {
  if (!el) return
  if (el instanceof HTMLVideoElement) {
    el.pause()
    el.removeAttribute('src')
    el.load()
  }
  el.remove()
}

// One listener, whatever the wallpaper: Page Visibility is the only signal the
// page gets for "the user cannot see this window right now".
let bgVisibilityBound = false
function bindBackgroundVisibility(): void {
  if (bgVisibilityBound) return
  bgVisibilityBound = true
  document.addEventListener('visibilitychange', () => {
    const el = activeEl
    if (!(el instanceof HTMLVideoElement)) return
    if (document.hidden) el.pause()
    else el.play().catch(() => {})
  })
}

function createBackgroundVideo(): HTMLVideoElement {
  const v = document.createElement('video')
  // The CSS selector accepts the class so a layer being loaded has its fixed
  // overlay geometry before it earns the #atlr-bg id (only the live layer may
  // hold that id, and the outgoing one is still on screen).
  v.classList.add('atlr-bg')
  v.autoplay = true
  v.loop = true
  v.muted = !currentBgVideoSound
  // The muted *attribute* is what lets an autoplaying video start without a
  // user gesture; the property alone is not enough on every engine.
  if (!currentBgVideoSound) v.setAttribute('muted', '')
  v.playsInline = true
  v.preload = 'auto'
  v.setAttribute('playsinline', '')
  v.setAttribute('aria-hidden', 'true')
  v.setAttribute('disablepictureinpicture', '')
  v.tabIndex = -1
  // A decoder that dies takes the layer with it: rebuild the node, guarded by
  // the same cooldown the periodic check uses.
  v.addEventListener('error', () => {
    if (v === activeEl) rebuildLayer()
  })
  return v
}

function createBackgroundImage(): HTMLImageElement {
  const img = document.createElement('img')
  img.classList.add('atlr-bg')
  img.alt = ''
  img.draggable = false
  return img
}

// Normalizes the same way getPack does, so the two can never disagree: an id
// the app does not ship would otherwise be persisted and written as an
// attribute no CSS matches, while getPack went on reporting the default.
export function setPack(pack: string): void {
  const id = normalizePack(pack)
  currentPack = id
  localStorage.setItem(PACK_KEY, id)
  apply(currentMode, id)
  void persistSelection()
}

// persistSelection mirrors the choice into Data/themes/selection.json so it
// follows the portable data directory across machines. Best effort: a failure
// (mobile reconnecting, offline) leaves the local state authoritative until
// the next successful write.
async function persistSelection(): Promise<void> {
  try {
    await saveThemeSelection(
      currentMode,
      currentPack,
      currentBackground,
      currentStartupAnim,
      { fit: currentBgFit, opacity: currentBgOpacity, blur: currentBgBlur },
      { id: currentBgVideo, sound: currentBgVideoSound },
    )
  } catch {
    // Kept locally only; retried on the next setMode/setPack/setBackground.
  }
}

// initTheme applies the persisted choice on boot. Call once at app start.
export function initTheme(): void {
  currentMode = (localStorage.getItem(MODE_KEY) as ThemeMode) || 'light'
  currentPack = normalizePack(localStorage.getItem(PACK_KEY))
  currentBackground = (localStorage.getItem(BG_KEY) as BackgroundID) || ''
  currentBgVideo = localStorage.getItem(BG_VIDEO_KEY) === 'sakura' ? 'sakura' : ''
  currentBgVideoSound = localStorage.getItem(BG_VIDEO_SOUND_KEY) === '1'
  // The server treats the two as exclusive; mirror that locally so a stale
  // image key left next to a video key cannot mount both layers.
  if (currentBgVideo) currentBackground = ''
  const fit = localStorage.getItem(BG_FIT_KEY) as BackgroundFit | null
  currentBgFit = fit === 'contain' || fit === 'fill' ? fit : 'cover'
  // Number(null) is 0, not NaN — guard on the raw string so "never stored"
  // keeps the defaults instead of clamping to the range floor.
  const opRaw = localStorage.getItem(BG_OPACITY_KEY)
  const op = opRaw == null ? NaN : Number(opRaw)
  currentBgOpacity = Number.isFinite(op) ? Math.min(0.8, Math.max(0.05, op)) : 0.2
  const blRaw = localStorage.getItem(BG_BLUR_KEY)
  const bl = blRaw == null ? NaN : Number(blRaw)
  currentBgBlur = Number.isFinite(bl) ? Math.min(30, Math.max(0, bl)) : 0
  currentStartupAnim = localStorage.getItem(ANIM_KEY) !== '0'
  apply(currentMode, currentPack)
  applyBackgroundLayer()
  startLayerGuard()
  // The splash owns the screen until it says it is done; only then may the
  // wallpaper unmute. Re-applying is cheap — the layer of the right kind is
  // reused and only its sound flag changes.
  if (typeof window !== 'undefined') {
    window.addEventListener('dsh-startup:done', () => { applyBackgroundLayer() }, { once: true })
  }
}

// loadThemeSelection reconciles the boot choice with the server copy under
// Data/themes/. Runs once, after loadUserPacks (a stored user-pack id can only
// be honoured once its stylesheet is linked). The server copy wins when one
// exists — that is what makes the theme migrate with the Data directory; when
// nothing is persisted yet, this is a genuine first run, and it lands on the
// rebuild's defaults: 少女 (blossom) in dark mode with the first wallpaper.
// A failure here leaves the boot state untouched — same reasoning as
// loadUserPacks.
export async function loadThemeSelection(): Promise<void> {
  let sel
  try {
    sel = await getThemeSelection()
  } catch {
    return
  }
  if (sel.mode || sel.pack) {
    currentMode = (['light', 'dark', 'system'].includes(sel.mode ?? '') ? sel.mode : 'light') as ThemeMode
    currentPack = normalizePack(sel.pack || DEFAULT_PACK)
    const bg = (sel.background ?? '') as BackgroundID
    currentBackground = bg === 'off' || bg === 'bloom' || bg === 'mystery' || bg === 'meadow' || bg === 'custom' ? bg : ''
    currentBgVideo = sel.backgroundVideo === 'sakura' ? 'sakura' : ''
    currentBgVideoSound = sel.backgroundVideoSound === true
    // Server-enforced exclusivity, applied here too so the boot state matches
    // what is about to be persisted back.
    if (currentBgVideo) currentBackground = ''
    currentStartupAnim = sel.startupAnim !== false
    const fit = (sel.backgroundFit ?? 'cover') as BackgroundFit
    currentBgFit = fit === 'contain' || fit === 'fill' ? fit : 'cover'
    const op = Number(sel.backgroundOpacity)
    currentBgOpacity = Number.isFinite(op) && sel.backgroundOpacity != null ? Math.min(0.8, Math.max(0.05, op)) : 0.2
    const bl = Number(sel.backgroundBlur)
    currentBgBlur = Number.isFinite(bl) && sel.backgroundBlur != null ? Math.min(30, Math.max(0, bl)) : 0
  } else {
    currentMode = FIRST_RUN_MODE
    currentPack = normalizePack(FIRST_RUN_PACK) || DEFAULT_PACK
    currentBackground = FIRST_RUN_BACKGROUND
    currentBgVideo = ''
    currentBgVideoSound = false
    currentStartupAnim = true
    currentBgFit = 'cover'
    currentBgOpacity = 0.2
    currentBgBlur = 0
  }
  localStorage.setItem(MODE_KEY, currentMode)
  localStorage.setItem(PACK_KEY, currentPack)
  localStorage.setItem(ANIM_KEY, currentStartupAnim ? '1' : '0')
  if (currentBackground && currentBackground !== 'off') localStorage.setItem(BG_KEY, currentBackground)
  else localStorage.removeItem(BG_KEY)
  if (currentBgVideo) localStorage.setItem(BG_VIDEO_KEY, currentBgVideo)
  else localStorage.removeItem(BG_VIDEO_KEY)
  localStorage.setItem(BG_VIDEO_SOUND_KEY, currentBgVideoSound ? '1' : '0')
  apply(currentMode, currentPack)
  applyBackgroundLayer()
  void persistSelection()
}

// linkThemeStylesheet appends a user theme's stylesheet. Order matters: the
// default dark palette (`:root[data-theme="dark"]` in app.css) has the same
// specificity as a pack's own block, so a theme only wins by coming later in
// the cascade — which appending to <head> guarantees, app.css being an import
// of the entry module.
function linkThemeStylesheet(id: string): void {
  if (document.querySelector(`link[data-octo-theme="${id}"]`)) return
  const link = document.createElement('link')
  link.rel = 'stylesheet'
  link.href = `/api/themes/${encodeURIComponent(id)}/theme.css`
  link.setAttribute('data-octo-theme', id)
  document.head.appendChild(link)
}

// loadUserPacks discovers the themes under ~/.octo/themes/, links them and
// re-applies the stored choice.
//
// The re-apply is the point: boot runs before this resolves, so a stored user
// theme id normalized to the default — getPack only reads, never writes back,
// so the choice itself survived and simply needs applying once its stylesheet
// is known. A user on a built-in pack sees nothing happen.
//
// A failure here is not worth surfacing: the app is fully usable on the
// built-in packs, and the picker just shows four entries instead of five.
export async function loadUserPacks(): Promise<void> {
  let themes
  try {
    themes = await listThemes()
  } catch {
    // A failed request says nothing about what is on disk, so the list is left
    // exactly as it is rather than being emptied.
    return
  }

  const choices: PackChoice[] = []
  for (const th of themes) {
    if (PACK_IDS.has(th.id)) continue
    linkThemeStylesheet(th.id)
    choices.push({
      id: th.id,
      label: th.name || th.id,
      labels: th.names,
      swatch:
        th.swatch && th.swatch.length === 2
          ? [th.swatch[0], th.swatch[1]]
          : NEUTRAL_SWATCH,
      author: th.author,
      homepage: th.homepage,
    })
  }

  // An empty answer is a real answer — the user deleted their themes — so the
  // list and the known ids are rebuilt either way, and the re-apply drops
  // anyone whose pack just stopped existing back to the default.
  userPackIDs = new Set(choices.map((c) => c.id))
  packs.set([...builtinChoices(), ...choices])
  apply(getMode(), getPack())
}
