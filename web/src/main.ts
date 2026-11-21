import './app.css'
import App from './App.svelte'
import { mount } from 'svelte'
import { initIcons } from './lib/icons'
import { initTheme, loadUserPacks, loadThemeSelection, whenBackgroundReady } from './lib/theme'
import { initFramelessDrag } from './lib/framelessDrag'
import { installArtifactThemeRefresh } from './lib/artifacts'
import { applyTitlebarLift } from './lib/nativeWindow'

// Register the <iconify-icon> element and its bundled icon data before first
// paint — and shut the door on Iconify's API, which the CDN build used to call
// per icon. Nothing about the UI reaches a third party any more.
initIcons()

// Apply the persisted theme before first paint so there's no light-mode flash.
initTheme()

// ── Boot splash (dsh-startup-screen, adapted) ──────────────────────────────
// The vendored plugin's client runtime (public/dsh-startup/) plays the boot
// animation over the app. The index.html inline script already handled the
// synchronous off-cases (localStorage toggle, same-tab-session replay); here
// the server-side toggle (Data/themes/selection.json, so it migrates with the
// portable directory) gets the final word, then the runtime script loads.
;(function bootSplash() {
  const root = document.getElementById('dsh-startup-root')
  const done = () => window.dispatchEvent(new CustomEvent('dsh-startup:done'))
  let handedOver = false
  // The start-up animation is the first of the three screen owners (animation,
  // still wallpaper, animated wallpaper). It leaves only after the wallpaper
  // picked for this launch has painted its first frame — normally already true
  // because the layer loads underneath the splash — so the handover never shows
  // a bare page. The 3s cap keeps a slow or absent wallpaper from pinning the
  // splash, and the flag makes repeated unlocks harmless.
  const unlock = () => {
    if (handedOver) return
    handedOver = true
    const cap = new Promise<void>((resolve) => window.setTimeout(resolve, 3000))
    void Promise.race([whenBackgroundReady(), cap]).then(() => handOver())
  }
  const handOver = () => {
    document.documentElement.classList.remove('dsu-lock')
    if (document.body) document.body.style.overflow = ''
    // Release the animation's audio/video before its DOM goes away: removing
    // the node alone can keep the decoder and its buffers alive until GC.
    const releaseMedia = () => {
      root?.querySelectorAll('audio, video').forEach((el) => {
        try {
          const media = el as HTMLMediaElement
          media.pause()
          media.removeAttribute('src')
          media.load()
        } catch { /* ignore */ }
      })
    }
    // Crossfade instead of a hard cut: the splash used to vanish in one
    // frame, which read as the app "appearing" rather than the boot screen
    // handing over. Pin it over the page while it fades so nothing shifts.
    const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
    if (!root || reduce) {
      releaseMedia()
      root?.remove()
      done()
      return
    }
    root.style.position = 'fixed'
    root.style.inset = '0'
    root.style.zIndex = '2147483000'
    root.style.pointerEvents = 'none'
    root.style.transition = 'opacity 280ms ease'
    // Two frames: one lets the styles above apply, the next starts the fade
    // from the opaque frame the splash ended on.
    requestAnimationFrame(() => {
      requestAnimationFrame(() => { root.style.opacity = '0' })
    })
    window.setTimeout(() => {
      releaseMedia()
      root.remove()
      done()
    }, 300)
  }
  if ((window as unknown as { __DSH_STARTUP_SKIP__?: boolean }).__DSH_STARTUP_SKIP__) return
  if (sessionStorage.getItem('dsh-startup-screen:shown') === '1') {
    unlock()
    return
  }
  // Every act advances only on a click / Enter / Space: requireInteraction is
  // always on and speed stays 1. No first-run fast-forward, no auto-advance.
  // First-run onboarding is gated in App.svelte (onboardPhase → FirstRunSetup),
  // NOT here: the splash plays on every launch, first one included.
  fetch('/api/theme-selection', { cache: 'no-store' })
    .then((r) => r.json())
    .then((sel) => {
      if (sel && sel.startupAnim === false) {
        localStorage.setItem('atlr.startupAnim', '0')
        unlock()
        return false
      }
      return true
    })
    .catch(() => true) // server unreachable: default the splash on
    .then((run) => {
      if (!run) return
      const w = window as unknown as Record<string, unknown>
      w.__DSH_STARTUP__ = {
        enabled: true,
        mode: 'session',
        speed: 1,
        requireInteraction: true,
        theme: document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light',
      }
      w.__DSH_STARTUP_BOOT__ = 'atlr'
      const js = document.createElement('script')
      js.src = '/dsh-startup/splash.js'
      js.onerror = unlock
      document.head.appendChild(js)
      // Guard only against "the splash never started at all" (script 404/blocked,
      // stylesheet missing so the timeline never paints). Judgement is
      // __DSH_STARTUP_RUNNING__, set by splash.js:1409 as the very first
      // statement of run() — before the first act paints. Waiting on a click
      // disarms the runtime's own 15s stall watchdog (splash.js:876-880), and
      // index.html's 9s failsafe bails out on the same flag, so once RUNNING is
      // true nothing here may time the animation out: a user is free to sit on
      // act 1 for as long as they like.
      const since = Date.now()
      const guard = window.setInterval(() => {
        if (w.__DSH_STARTUP_RUNNING__ === true) {
          window.clearInterval(guard)
          return
        }
        if (Date.now() - since < 10_000) return
        window.clearInterval(guard)
        try {
          ;(
            window as unknown as { __DSH_STARTUP_API__?: { finish?: () => void } }
          ).__DSH_STARTUP_API__?.finish?.()
        } catch {
          /* the failsafe must not throw */
        }
      }, 250)
      window.addEventListener('dsh-startup:done', () => window.clearInterval(guard), { once: true })
    })
})()

// Discover the user's own themes (~/.octo/themes) in the background. A stored
// id belonging to one of them cannot be honoured before its stylesheet exists,
// so loadUserPacks re-applies the choice once it has linked them; a user on a
// built-in pack never notices this ran. The server-side selection (Data/
// themes/selection.json) is reconciled afterwards — it is the portable source
// of truth and may point at one of the discovered user packs.
void loadUserPacks().then(loadThemeSelection)

// Desktop shell on macOS: pin the titlebar rows' axis to the traffic lights
// before first paint — the inset and padding would otherwise wait on
// /api/version and flash the rows crammed under the lights at startup.
// No-op elsewhere.
applyTitlebarLift()

// Rebuild baked-theme artifact previews whenever the resolved theme changes.
installArtifactThemeRefresh()

// Desktop shell on Windows/Linux: window drag + edge resize (no-op elsewhere).
initFramelessDrag()

const app = mount(App, { target: document.getElementById('app')! })

export default app
