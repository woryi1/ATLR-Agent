package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-octo/octo-agent/internal/brand"
	"github.com/open-octo/octo-agent/internal/server"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// nativeBridge implements server.NativeBridge on top of the Wails runtime. The
// server calls it (from HTTP-handler goroutines) for the capabilities a
// browser can't provide; Wails' own APIs marshal to the UI thread internally,
// so no manual main-thread dispatch is needed here.
type nativeBridge struct {
	app      *application.App
	window   *application.WebviewWindow
	notifier *notifications.NotificationService // nil unless bundled
	// srv is the in-process hub, set once bound. Atomic because startHub (the
	// ApplicationStarted goroutine) writes it while the tray-refresh loop reads it.
	srv atomic.Pointer[server.Server]
	// url is the hub's base URL, "http://127.0.0.1:8088"-style. Written by main
	// and by startHub (the ApplicationStarted goroutine) while notification and
	// window goroutines read it, so it's atomic like srv.
	url atomic.Value // string

	// closeLog releases the rotating serve.log writer that startHub installs once
	// this process owns the port. Written by startHub (the ApplicationStarted
	// goroutine) and read by main's post-Run cleanup on another goroutine, so it's
	// atomic like srv.
	closeLog atomic.Pointer[func()]

	// allowQuit gates the app's ShouldQuit on Windows/Linux, where closing the
	// last window would otherwise terminate the app (and the hub with it). It
	// starts false when KeepRunningInBackground is on, so a window close hides
	// to the tray; requestQuit flips it true for a real quit. On macOS the
	// close behavior itself is the ApplicationShouldTerminateAfterLastWindowClosed
	// option (ShouldQuit there always allows the quit), but the flag still tells
	// the close hook whether the app is staying alive — see closeShouldHide.
	allowQuit atomic.Bool
	// hidden is set when a close was turned into a hide and cleared by the next
	// show. The liveness probe consults it: a window the user just put away
	// must not be replaced — and popped back up — because its page went quiet.
	hidden atomic.Bool

	// Webview liveness, fed by POST /api/native/heartbeat from the page in
	// nativeShell mode (see startNativeHeartbeat in the frontend). All unix
	// nanos, atomic: HTTP-handler goroutines write, the show/probe paths read.
	// lastBeat is when a beat last arrived (JS provably alive then).
	// lastFrame is when the page last observed a requestAnimationFrame tick
	// (the render pipeline provably alive then). lastHiddenBeat is when the
	// page last reported itself hidden — it owes no frames then, so a missing
	// frame must not read as a black window. Together they separate the three
	// states judgePage distinguishes: healthy, black, and silent.
	// probeInFlight collapses concurrent probes; lastRevive rate-limits
	// re-creation.
	lastBeat       atomic.Int64
	lastFrame      atomic.Int64
	lastHiddenBeat atomic.Int64
	// lastFocusBeat is when the page last beat while visible AND holding the
	// foreground slot. The black-window watchdog only acts on windows in that
	// state; a backgrounded or locked session is entitled to stop painting.
	lastFocusBeat atomic.Int64
	probeInFlight atomic.Bool
	lastRevive    atomic.Int64
	// watchOnce makes the black-window watchdog a single goroutine per app.
	watchOnce sync.Once

	// creating guards window CREATION (not the pointer, which windowMu covers):
	// two shows racing on a nil window would each build one, leaving an orphan
	// that the frontend's close button can't reach on the frameless platforms.
	// A CAS rather than a mutex on purpose — showWindowAt can run on the UI
	// thread (a notification click), and a lock held across the InvokeSync in
	// window creation would deadlock against it.
	creating atomic.Bool

	// Test seams: testProbeDelay shortens probeAfterShow's sleep, reviveFn
	// substitutes the window swap. Both zero/nil in production.
	testProbeDelay time.Duration
	reviveFn       func(old *application.WebviewWindow)

	// windowMu guards every read and write of b.window that could race the
	// pointer being REPLACED: showWindowAt's snapshot, the liveness probe's
	// background goroutine, the WindowClosing handler that clears it, and the
	// read-only accessors (Minimise, Close, WindowState…), which load through
	// currentWindow now that the probe can swap the pointer autonomously. Held
	// only around the pointer access, never across a window method — those
	// marshal to the UI thread and would deadlock under a lock.
	windowMu sync.Mutex

	// notifySeq makes each Notify call's notification identifier unique. macOS
	// rejects an addNotificationRequest with an empty identifier (its completion
	// handler errors and nothing is delivered), and a shared constant id would
	// make each new toast silently replace the previous one, so every call gets
	// its own.
	notifySeq atomic.Uint64

	// tray is the system-tray handle, stored so a tray rebuild can happen
	// immediately rather than waiting for refreshTrayLoop's next tick.
	tray atomic.Pointer[application.SystemTray]

	// pet is the desktop-pet window, nil while it is down. An auxiliary window
	// with no part in the main window's show/hide/revive machinery — see pet.go.
	pet atomic.Pointer[application.WebviewWindow]
	// petState is the pet's current animation state, published by setPetState
	// and read by the pointer loop, whose hit shapes depend on the pose.
	petState atomic.Pointer[string]

	// webWin is the auxiliary web page window used for "open in app" targets
	// (a music track's Bilibili page, for instance). It is independent of the
	// main window: closing it destroys it, and nothing revives it.
	webWin atomic.Pointer[application.WebviewWindow]

	settingsMu sync.Mutex
	settings   desktopSettings
	// geomTimer debounces the optional window-size write. Guarded by settingsMu.
	geomTimer *time.Timer
	// petGeomTimer debounces persistence of the pet's position — a drag shows up
	petGeomTimer *time.Timer
}

// Built-in window size for a first launch (no saved geometry yet).
const (
	defaultWindowWidth  = 1280
	defaultWindowHeight = 860
)

// Floor on the native window itself. Below this the CSS-level minimums inside
// the webview (sidebarWidth.ts's CENTER_MIN, PANEL_MIN) can't do their job —
// they only stop the resizable columns from squeezing the conversation
// column, not the OS window from shrinking out from under all of them. 704 =
// the rail-mode sidebar (64px, the width the sidebar collapses to below the
// 860px breakpoint) plus CENTER_MIN (640px, see sidebarWidth.ts) with the
// artifacts panel closed — the narrowest state the layout is ever meant to
// reach.
const (
	minWindowWidth  = 704
	minWindowHeight = 480
)

// defaultWindowSizeFor sizes a first launch against the primary screen's work
// area instead of a fixed 1280×860. On a small school laptop that fixed size is
// taller than the screen, which pushes the frameless titlebar buttons above the
// visible area and leaves them unreachable.
func defaultWindowSizeFor(app *application.App) (int, int) {
	const golden = 1.618
	const maxHeight = 980
	if app != nil && app.Screen != nil {
		if s := app.Screen.GetPrimary(); s != nil {
			if s.WorkArea.Width > 0 && s.WorkArea.Height > 0 {
				// Height drives the size, width follows the golden ratio, and both
				// are clamped inside the work area. That keeps the same proportions
				// on every panel instead of the wide "stretched" shape a percentage
				// of both axes produces on a widescreen display.
				h := int(float64(s.WorkArea.Height) * 0.88)
				if h > maxHeight {
					h = maxHeight
				}
				if h < minWindowHeight {
					h = minWindowHeight
				}
				w := int(float64(h) * golden)
				maxW := int(float64(s.WorkArea.Width) * 0.92)
				if w > maxW {
					w = maxW
					h = int(float64(w) / golden)
				}
				return clampToWorkArea(s, w, h)
			}
		}
	}
	return defaultWindowWidth, defaultWindowHeight
}

// clampToWorkArea keeps a remembered geometry inside the current screen. A
// window restored at 1920×1080 on a 1366×768 panel would otherwise open with
// its top edge (and the frameless controls) off screen.
func clampToWorkArea(s *application.Screen, width, height int) (int, int) {
	if s == nil || s.WorkArea.Width <= 0 || s.WorkArea.Height <= 0 {
		return width, height
	}
	maxW := int(float64(s.WorkArea.Width) * 0.95)
	maxH := int(float64(s.WorkArea.Height) * 0.95)
	if width > maxW {
		width = maxW
	}
	if height > maxH {
		height = maxH
	}
	// The floor must not win over a screen smaller than it: an oversized window
	// is exactly the off-screen case this function exists to prevent.
	if width < minWindowWidth && maxW >= minWindowWidth {
		width = minWindowWidth
	}
	if height < minWindowHeight && maxH >= minWindowHeight {
		height = minWindowHeight
	}
	return width, height
}

// desktopShellQuery marks the window's URL so the frontend can tell it is
// running inside the desktop-shell webview rather than an external browser
// pointed at the same hub. The hub reports native=true to every client (the
// NativeBridge is a server-wide capability), but only the shell webview should
// behave as "native" — use the OS file dialog, route notifications through the
// OS, inset the header past the traffic lights. An external browser on this
// machine keys off the absent marker and stays plain web. The frontend reads it
// from location.search (see VersionBadge.svelte).
const desktopShellQuery = "shell=octo-desktop"

// setURL records the hub's base URL; baseURL reads it ("" until set). Both are
// safe from any goroutine — see the url field.
func (b *nativeBridge) setURL(u string) { b.url.Store(u) }

func (b *nativeBridge) baseURL() string {
	if v := b.url.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// shellURL builds the desktop-shell window URL for a frontend route hash,
// always carrying the desktopShellQuery marker. base is b.baseURL(), e.g.
// "http://127.0.0.1:8088". Fresh-window loads and SetURL navigations share it,
// so they produce the identical path+query and a route change stays a pure
// hashchange (no reload). The exact query string is contracted with the
// frontend reader isDesktopShell in web/src/lib/stores.ts — keep both sides in
// sync (TestShellURL pins the Go side).
func shellURL(base, hash string) string {
	u := base + "/?" + desktopShellQuery
	// The shell webview aligns its titlebar rows to the traffic lights, whose
	// position depends on the host's macOS version (26pt from the window top
	// on macOS 26 for this window style, 20px through macOS 15) — and it
	// needs that version at first paint: waiting for /api/version leaves the
	// rows un-inset under the lights for a second or two at startup. So the
	// version rides in the URL alongside the shell marker; the frontend seeds
	// macosMajor from it (web/src/lib/stores.ts) and re-confirms from
	// /api/version's os_version once that lands.
	if runtime.GOOS == "darwin" {
		if major, _, _ := strings.Cut(server.OSVersion(), "."); major != "" {
			u += "&macos=" + major
		}
	}
	if hash != "" {
		u += "#" + hash
	}
	return u
}

// rememberWindowGeometry stores the window's normal-state size when the user
// opted into remembering it. The window is read here, in the resize handler
// where it is guaranteed alive; maximised/fullscreen/minimised sizes are never
// recorded because they are not the restore size.
func (b *nativeBridge) rememberWindowGeometry(w *application.WebviewWindow) {
	b.settingsMu.Lock()
	enabled := b.settings.RememberWindowSize
	b.settingsMu.Unlock()
	if !enabled {
		return
	}
	maximised := w.IsMaximised()
	fullscreen := w.IsFullscreen()
	minimised := w.IsMinimised()
	width, height := w.Size()

	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	if !maximised && !fullscreen && !minimised && width > 0 && height > 0 {
		b.settings.WindowWidth = width
		b.settings.WindowHeight = height
	}
	if b.geomTimer != nil {
		b.geomTimer.Stop()
	}
	b.geomTimer = time.AfterFunc(400*time.Millisecond, b.persistSettings)
}

// WindowSizeRemembered reports the opt-in window-size setting.
func (b *nativeBridge) WindowSizeRemembered() (bool, error) {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	return b.settings.RememberWindowSize, nil
}

// SetWindowSizeRemembered enables or disables the remembered window size. When
// disabling, the stored geometry is dropped so re-enabling later cannot restore
// a stale size from a different monitor.
func (b *nativeBridge) SetWindowSizeRemembered(enable bool) error {
	// Window methods marshal to the UI thread, so read them BEFORE taking the
	// settings lock (never hold settingsMu across a window call).
	var seedW, seedH int
	if enable {
		if win := b.currentWindow(); win != nil && !win.IsMaximised() && !win.IsFullscreen() && !win.IsMinimised() {
			seedW, seedH = win.Size()
		}
	}
	b.settingsMu.Lock()
	b.settings.RememberWindowSize = enable
	if !enable {
		b.settings.WindowWidth = 0
		b.settings.WindowHeight = 0
	} else if seedW > 0 && seedH > 0 {
		// Seed the first restore point from the window that is on screen now.
		b.settings.WindowWidth, b.settings.WindowHeight = seedW, seedH
	}
	b.settingsMu.Unlock()
	b.persistSettings()
	return nil
}

// persistSettings writes the current settings to disk. It touches no window
// state, so it is safe to call from the debounce timer or the closing window.
func (b *nativeBridge) persistSettings() {
	b.settingsMu.Lock()
	snapshot := b.settings
	b.settingsMu.Unlock()
	_ = saveDesktopSettings(snapshot)
}

// currentWindow loads the window pointer under windowMu. Callers must not
// hold windowMu across any method on the returned window — those marshal to
// the UI thread and would deadlock under the lock. A liveness probe can still
// detach and close the window between the load and the call; that sliver is
// unavoidable without locking across UI-thread marshalling.
func (b *nativeBridge) currentWindow() *application.WebviewWindow {
	b.windowMu.Lock()
	defer b.windowMu.Unlock()
	return b.window
}

// PickFolder opens the OS directory-choose dialog and returns the chosen path.
// PromptForSingleSelection returns "" when the user cancels, which we surface
// as cancelled=true so the caller leaves the working dir untouched.
func (b *nativeBridge) PickFolder(_ context.Context, startDir string) (string, bool, error) {
	dlg := b.app.Dialog.OpenFile().
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true)
	// Attach to the window so it opens as a sheet. A detached panel (Wails'
	// beginWithCompletionHandler path when no window is set) stops responding
	// to its buttons after the user switches away from the app and back.
	if win := b.currentWindow(); win != nil {
		dlg.AttachToWindow(win)
	}
	if startDir != "" {
		dlg.SetDirectory(startDir)
	}
	path, err := dlg.PromptForSingleSelection()
	if err != nil {
		return "", false, err
	}
	if path == "" {
		return "", true, nil
	}
	return path, false, nil
}

// PickFile opens the OS file-choose dialog and returns the chosen path,
// cancelled when dismissed.
func (b *nativeBridge) PickFile(_ context.Context, startDir string) (string, bool, error) {
	dlg := b.app.Dialog.OpenFile().
		CanChooseFiles(true).
		CanChooseDirectories(false)
	if win := b.currentWindow(); win != nil { // sheet, not a detached panel — see PickFolder.
		dlg.AttachToWindow(win)
	}
	if startDir != "" {
		dlg.SetDirectory(startDir)
	}
	path, err := dlg.PromptForSingleSelection()
	if err != nil {
		return "", false, err
	}
	if path == "" {
		return "", true, nil
	}
	return path, false, nil
}

// SaveFile shows the OS save dialog seeded with defaultName, writes content to
// the chosen path, and returns it. cancelled when dismissed. This is how the
// artifact "Download" action lands a file on disk in the desktop shell — the
// octo-served page has no webview download delegate, so an in-page blob
// download does nothing.
func (b *nativeBridge) SaveFile(_ context.Context, defaultName, content string) (string, bool, error) {
	dlg := b.app.Dialog.SaveFile().CanCreateDirectories(true)
	if win := b.currentWindow(); win != nil { // sheet, not a detached panel — see PickFolder.
		dlg.AttachToWindow(win)
	}
	if defaultName != "" {
		dlg.SetFilename(defaultName)
	}
	path, err := dlg.PromptForSingleSelection()
	if err != nil {
		return "", false, err
	}
	if path == "" {
		return "", true, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", false, err
	}
	return path, false, nil
}

// Print opens the OS print dialog for the window's current content — how the
// transcript's PDF export lands a file, since the shell can't be relied on to
// honour an in-page window.print(). Wails prints natively on macOS and forwards
// to window.print() on Windows and Linux.
//
// Non-blocking: on macOS the print panel runs as a window sheet, so this
// returns while it is still open and the page must keep its print layout up.
// No-op before the window exists.
func (b *nativeBridge) Print() error {
	win := b.currentWindow()
	if win == nil {
		return nil
	}
	return win.Print()
}

// Notify raises an OS-native notification. No-op when the notifications service
// isn't available (an unbundled dev binary — the service needs a bundle id).
// Best-effort by contract: a delivery failure (e.g. permission not yet granted)
// is swallowed.
func (b *nativeBridge) Notify(title, body string) {
	if b.notifier == nil {
		return
	}
	_ = b.notifier.SendNotification(notifications.NotificationOptions{
		ID:    fmt.Sprintf("octo-notify-%d", b.notifySeq.Add(1)),
		Title: title,
		Body:  body,
	})
}

// NotifySession raises an OS-native notification that, when clicked, brings the
// user to the given session. The session ID travels in the notification's Data
// payload and is echoed back as UserInfo in the platform response handler.
//
// The notification ID is deterministic per session, not the shared sequence:
// a repeat notification for the same session replaces the previous one instead
// of stacking in the notification center, and DismissSessionNotification can
// address it later once the user has seen the session.
func (b *nativeBridge) NotifySession(title, body, sessionID string) {
	if b.notifier == nil {
		return
	}
	_ = b.notifier.SendNotification(notifications.NotificationOptions{
		ID:    sessionNotificationID(sessionID),
		Title: title,
		Body:  body,
		Data: map[string]any{
			"session_id": sessionID,
		},
	})
}

func sessionNotificationID(sessionID string) string {
	return "octo-notify-session-" + sessionID
}

// DismissSessionNotification retracts the delivered (and any pending)
// notification for a session — the user has opened the session in the app, so
// the OS notification has done its job. Best-effort like NotifySession: on
// Windows RemoveDeliveredNotification is a platform stub (wintoast cannot
// retract from the Action Center), so dismissal is a no-op there.
func (b *nativeBridge) DismissSessionNotification(sessionID string) {
	if b.notifier == nil {
		return
	}
	id := sessionNotificationID(sessionID)
	_ = b.notifier.RemovePendingNotification(id)
	_ = b.notifier.RemoveDeliveredNotification(id)
}

// requestNotificationAuthorization asks the OS for permission to post
// notifications, without which every Notify call silently no-ops. macOS
// requires this at runtime: UNUserNotificationCenter drops delivery (reporting
// no error) until the user has granted authorization, so the first call raises
// the system permission prompt and blocks until they answer — run it off the UI
// thread. Windows/Linux grant immediately. No-op without a notifier.
func (b *nativeBridge) requestNotificationAuthorization() {
	if b.notifier == nil {
		return
	}
	_, _ = b.notifier.RequestNotificationAuthorization()
}

// refreshTray rebuilds and re-publishes the tray menu right now, so a state
// change (e.g. the pet appearing) shows without waiting for refreshTrayLoop's
// next tick. No-op until the tray handle has been stored. SetMenu marshals to
// the UI thread, so it's safe to call from another goroutine.
func (b *nativeBridge) refreshTray() {
	if t := b.tray.Load(); t != nil {
		t.SetMenu(buildTrayMenu(b.app, b))
	}
}

// AutostartEnabled reports whether the app is registered to launch at login.
func (b *nativeBridge) AutostartEnabled() (bool, error) {
	st, err := b.app.Autostart.Status()
	if err != nil {
		return false, err
	}
	return st.Enabled, nil
}

// SetAutostart registers or unregisters the app from launch-at-login.
func (b *nativeBridge) SetAutostart(enable bool) error {
	if enable {
		return b.app.Autostart.Enable()
	}
	return b.app.Autostart.Disable()
}

// ToggleMaximise maximises or restores the window (the double-click-titlebar
// zoom the frontend can't trigger itself). No-op before the window exists.
func (b *nativeBridge) ToggleMaximise() {
	if win := b.currentWindow(); win != nil {
		win.ToggleMaximise()
	}
}

// Minimise minimises the window to the taskbar/dock. No-op before the window exists.
func (b *nativeBridge) Minimise() {
	if win := b.currentWindow(); win != nil {
		win.Minimise()
	}
}

// Close closes the window (sends WindowClosing; the close hook then hides the
// window when the hub keeps running in the tray, otherwise it is destroyed and
// the app's ShouldQuit decides whether the process terminates). No-op before
// the window exists.
func (b *nativeBridge) Close() {
	if win := b.currentWindow(); win != nil {
		win.Close()
	}
}

// WindowState reports whether the window is currently maximised. Returns false
// before the window exists.
func (b *nativeBridge) WindowState() bool {
	win := b.currentWindow()
	if win == nil {
		return false
	}
	return win.IsMaximised()
}

// showWindow brings the hub window to the foreground on the current view.
func (b *nativeBridge) showWindow() { b.showWindowAt("") }

// openSettings brings the window up and opens the Settings modal (tray
// "Settings"). The frontend maps the "settings" hash route to the modal.
func (b *nativeBridge) openSettings() { b.showWindowAt("settings") }

// openNewSession brings the window up and starts a new session (tray
// "New Session" / a keyboard-⌘N-equivalent from the shell side). The frontend
// maps the "new" hash route to createNewSession(), the same action the in-app
// "New Session" button and Cmd/Ctrl+N trigger.
func (b *nativeBridge) openNewSession() { b.showWindowAt("new") }

// showWindowAt brings the hub window to the foreground — un-hiding the one a
// close put in the tray (KeepRunningInBackground) or re-creating it if it was
// destroyed — and navigates to the given frontend hash route (empty = leave it
// where it is). The frontend routes on location.hash, so a fresh window loads
// straight into the view and an existing one navigates via a hashchange — no
// full reload.
func (b *nativeBridge) showWindowAt(hash string) {
	// The marker rides on every navigation the shell performs (fresh window and
	// SetURL alike) so nativeShell stays true across reloads and route changes.
	target := shellURL(b.baseURL(), hash)
	// Snapshot the pointer once: the frame probe's goroutine can clear it
	// concurrently, and a lock-free re-read mid-function could see that nil and
	// panic. Everything below works off win, then publishes it back.
	win := b.currentWindow()
	created := false
	if win == nil {
		if b.app == nil || b.baseURL() == "" {
			return // not bound yet
		}
		// Only one goroutine builds the window. A loser re-reads the pointer:
		// if the winner has already published one it just shows that, otherwise
		// it leaves the work to the winner, which shows the window itself.
		if !b.creating.CompareAndSwap(false, true) {
			win = b.currentWindow()
			if win == nil {
				return
			}
		} else {
			defer b.creating.Store(false)
			created = true
		}
	}
	if created {
		// Size to the current screen by default. If the user opted into
		// remembering the window size, restore the last windowed geometry —
		// clamped to this screen so a size from a bigger monitor cannot open
		// partly off screen.
		b.settingsMu.Lock()
		remember := b.settings.RememberWindowSize
		width, height := b.settings.WindowWidth, b.settings.WindowHeight
		b.settingsMu.Unlock()
		if !remember || width <= 0 || height <= 0 {
			width, height = defaultWindowSizeFor(b.app)
		} else if s := b.app.Screen.GetPrimary(); s != nil {
			width, height = clampToWorkArea(s, width, height)
		}
		w := b.app.Window.NewWithOptions(application.WebviewWindowOptions{
			Title:      brand.Name,
			Width:      width,
			Height:     height,
			MinWidth:   minWindowWidth,
			MinHeight:  minWindowHeight,
			StartState: application.WindowStateNormal,
			URL:        target,
			// Mac keeps its native title bar (hidden, inset traffic lights)
			// instead of Frameless, so the real NSWindow buttons — and their
			// native hover/zoom/tiling-menu behaviour — render for free.
			// InvisibleTitleBarHeight is deliberately left unset: that hack
			// natively swallows every left-mouse-down in its strip (including
			// double-clicks) before the DOM ever sees them, which broke
			// double-click-to-zoom the first time a native title bar was
			// tried. Window dragging instead goes through the same JS gesture
			// detection (framelessDrag.ts) Windows/Linux use — it isn't
			// mac-exclusive, so double-clicks reach the DOM normally there.
			// Windows/Linux stay Frameless with the frontend's own controls.
			Frameless: runtime.GOOS != "darwin",
			Mac: application.MacWindow{
				TitleBar: application.MacTitleBarHiddenInset,
			},
		})
		// Closing while the hub keeps running in the tray hides the window
		// instead of destroying it, so the next show is instant: destroying
		// meant every reopen paid for a new WebContent process, a cold parse of
		// the bundle and the full boot sequence (auth, config, session list,
		// route restore). A hook, not a listener: Wails runs hooks synchronously
		// before its own WindowClosing listener (the one that destroys the
		// window) and skips every listener once the event is cancelled —
		// including ours below — whereas listeners run concurrently and could
		// not reliably veto the destroy. Every reason the window must really
		// go away passes straight through: a quit in progress (allowQuit,
		// updateRestart) and a revive discarding a detached corpse (b.window
		// no longer points here). A fullscreen window also takes the old
		// destroy path: ordering out a fullscreen NSWindow is a known AppKit
		// trap (an empty Space left behind, a confused re-show) — exactly the
		// blank-window class this shell has fought before. Liveness is
		// otherwise unaffected — a hidden window that lost its renderer is
		// reloaded by the terminate handler below, and the next show still
		// runs probeAfterShow.
		w.RegisterHook(events.Common.WindowClosing, func(ev *application.WindowEvent) {
			if !closeShouldHide(b.currentWindow() == w, b.allowQuit.Load()) {
				return
			}
			if w.IsFullscreen() {
				return
			}
			ev.Cancel()
			b.hidden.Store(true)
			w.Hide()
		})
		// Forget the window when it really closes so a later Show re-creates one;
		// the app itself stays alive via ApplicationShouldTerminateAfterLastWindowClosed.
		// Flush settings (the pet position and background-running flag); the window
		// size is not part of them.
		w.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
			b.persistSettings()
			// Guarded: when a revive replaced this window, b.window already
			// points at the successor and must survive the old one's close.
			b.windowMu.Lock()
			replaced := b.window != w
			if !replaced {
				b.window = nil
			}
			b.windowMu.Unlock()
			// Windows only: the framework's own last-window-close quit is
			// suppressed (see DisableQuitOnLastWindowClosed in main.go), so a
			// user who opted out of background running needs the quit performed
			// here. Never for a revive's discarded window — that close is the
			// shell repairing itself, not the user leaving.
			if !replaced && closeShouldQuit(runtime.GOOS, b.allowQuit.Load()) {
				b.app.Quit()
			}
		})
		// Capture the windowed size when the user opted into remembering it.
		w.OnWindowEvent(events.Common.WindowDidResize, func(*application.WindowEvent) {
			b.rememberWindowGeometry(w)
		})
		// macOS kills WKWebView's WebContent process under memory pressure,
		// leaving a permanently blank window — nothing reloads it on its own.
		// Navigate back to the hub root to revive the page (SetURL for the
		// same reason as below: the page is octo-served, so the Wails runtime
		// and ExecJS are unavailable). Windows' equivalent (WebView2
		// ProcessFailed) isn't reachable through Wails — prevention lives in
		// the browser flags set in main.go instead. The b.window guard skips
		// a terminate that races window close: Wails' SetURL has no
		// isDestroyed check, so it could otherwise touch a freed NSWindow.
		if runtime.GOOS == "darwin" {
			w.OnWindowEvent(events.Mac.WebViewWebContentProcessDidTerminate, func(*application.WindowEvent) {
				if b.currentWindow() != w {
					return
				}
				w.SetURL(shellURL(b.baseURL(), ""))
			})
		}
		win = w
		b.windowMu.Lock()
		b.window = w
		b.windowMu.Unlock()
		// A window can go black while it is simply sitting there, with no show
		// to probe after; the watchdog is what repairs that case.
		b.watchBlackWindow()
	} else {
		// Re-validate the snapshot before touching the window: a liveness
		// probe may have detached and closed it meanwhile (its revive path
		// brings up the replacement itself, so there is nothing to do here).
		// SetURL on a destroyed window touches a freed NSWindow on macOS — the
		// hazard the terminate handler in the create path guards against. The
		// check cannot close the race entirely — the corpse is closed outside
		// windowMu, and holding the lock across a window method would deadlock
		// on the UI thread — but it shrinks it to the sliver between this
		// check and the first method call.
		if b.currentWindow() != win {
			return
		}
		if hash != "" {
			// Already open — navigate to the route. ExecJS can't be used here:
			// the page is served by octo's own server, not Wails' asset server,
			// so the Wails runtime never loads and ExecJS stays queued forever.
			// SetURL is a native navigation that doesn't depend on it.
			win.SetURL(target)
		}
	}
	// Cleared before Show so the probe below judges a window the user asked for.
	b.hidden.Store(false)
	win.Show()
	// Only un-minimise here. Wails' Restore() also un-maximises (and exits
	// fullscreen), so calling it unconditionally on every show/reopen — e.g.
	// clicking the dock icon to return to a maximised window — would shrink the
	// window back to its launch size. Guard on IsMinimised so a visible
	// maximised/fullscreen window keeps its size.
	if win.IsMinimised() {
		win.Restore()
	}
	win.Focus()
	// The probe anchors its verdict to this moment: a page only owes evidence
	// of life AFTER it was asked to come up. A freshly created window is
	// exempt — its first load can outlast the probe window, and if it never
	// comes up the next show judges it.
	if !created {
		b.probeAfterShow(win)
	}
}

// Webview liveness timings. Every verdict is reached from evidence produced
// AFTER a show, never from how long the page was quiet before one: a page has
// many legitimate reasons to go silent in the background (system sleep freezes
// it wholesale, Chromium throttles a long-hidden page's timers to ~1/minute)
// and destroying a healthy window is far worse than a few more seconds of
// black. probeDelay therefore only has to outlast two scheduled beats, so a
// page that came back has certainly reported in.
const (
	heartbeatInterval   = 5 * time.Second // matches BEAT_MS in web/src/lib/nativeHeartbeat.ts
	frameProbeDelay     = 3 * heartbeatInterval
	reviveCooldown      = 90 * time.Second
	maxReportedFrameAge = 10 * time.Minute // clamp: a bad/hostile beat must not move lastFrame far
)

// closeShouldQuit reports whether closing the window must terminate the app.
// Only Windows needs this: mac routes last-window-close through
// ApplicationShouldTerminateAfterLastWindowClosed and Linux's
// unregisterWindow consults ShouldQuit, but the Windows backend's own quit is
// suppressed (DisableQuitOnLastWindowClosed) because it fired unconditionally
// — killing a hub the user wanted kept alive. allowQuit is false exactly when
// the user has KeepRunningInBackground on, so closing hides to the tray.
func closeShouldQuit(goos string, allowQuit bool) bool {
	return goos == "windows" && allowQuit
}

// trayClickOpensWindow reports whether a left click on the tray icon should
// bring the window up.
//
// Windows only, and the platform's own convention rather than a preference:
// the menu is on right-click there, so left-click is free and is what every
// tray-resident app opens its window with. Without it a window closed to the
// tray can only be reached through the menu's "Show Octo".
//
// Not macOS: the status item's left click IS how its menu opens, so taking it
// would leave the menu unreachable. Not Linux either, though for a subtler
// reason — its tray runs the click handler when the MENU opens as well, so
// right-clicking to reach the menu would raise the window as a side effect.
func trayClickOpensWindow(goos string) bool {
	return goos == "windows"
}

// closeShouldHide reports whether a WindowClosing should be turned into a hide.
// Only the live window (current) qualifies — a detached corpse a revive is
// discarding must really close — and only while the app is staying alive:
// allowQuit means the close is meant to end the app (or requestQuit already
// decided so). No flag is needed for an ordinary quit:
// Wails' shutdown nils its window map before the closes it issues are
// dispatched, so those never reach the hook.
func closeShouldHide(current, allowQuit bool) bool {
	return current && !allowQuit
}

// reviveAllowed rate-limits automatic window re-creation so a page that is
// broken for a persistent reason (server wedged, port hijacked) can't put the
// shell into a destroy/create loop.
func (b *nativeBridge) reviveAllowed(now time.Time) bool {
	last := b.lastRevive.Load()
	return last == 0 || now.Sub(time.Unix(0, last)) > reviveCooldown
}

// pageVerdict is what the evidence collected after a show says about the page.
type pageVerdict int

const (
	// pageHealthy: it painted, or it reported itself hidden and owes no frames.
	pageHealthy pageVerdict = iota
	// pageBlack: it beats while claiming to be visible but has never painted
	// since the show — the black-window signature. Unambiguous, act at once.
	pageBlack
	// pageSilent: no beat at all. Usually a dead renderer, but a wedged hub or
	// a main thread stuck on a huge render looks identical from here, and both
	// of those recover on their own — so this verdict earns a second look
	// before the window is destroyed.
	pageSilent
)

// judgePage reads the post-show evidence. Nothing before shownAt counts: a page
// has ordinary reasons to be silent in the background (system sleep freezes it,
// Chromium throttles a long-hidden page's timers), and pre-show silence must
// never cost a healthy window.
func (b *nativeBridge) judgePage(shownAt time.Time) pageVerdict {
	after := func(v *atomic.Int64) bool {
		ns := v.Load()
		return ns != 0 && time.Unix(0, ns).After(shownAt)
	}
	if after(&b.lastFrame) || after(&b.lastHiddenBeat) {
		return pageHealthy
	}
	if after(&b.lastBeat) {
		return pageBlack
	}
	return pageSilent
}

// detachStalled detaches w when it is still the current window and a revive is
// off cooldown. Identity-keyed so a probe that slept through the user closing
// the window — or through another revive — leaves the successor alone. The
// caller closes the returned window; detaching first is what lets the
// replacement survive the old window's WindowClosing handler.
func (b *nativeBridge) detachStalled(w *application.WebviewWindow, now time.Time) *application.WebviewWindow {
	b.windowMu.Lock()
	defer b.windowMu.Unlock()
	if b.window != w || !b.reviveAllowed(now) {
		return nil
	}
	b.lastRevive.Store(now.UnixNano())
	b.window = nil
	return w
}

// probeAfterShow schedules the liveness verdict for a window that was already
// open when it was shown — the two failure modes a Show can't fix by itself:
// a renderer that died (no beats at all) and a compositor that died while JS
// kept running (beats, no frames — WebView2 across sleep/wake). The window is
// replaced rather than reloaded because a broken WebView2 surface survives
// navigation. Runs off the UI thread; the window methods it calls marshal
// themselves.
func (b *nativeBridge) probeAfterShow(shown *application.WebviewWindow) {
	if shown == nil {
		return
	}
	if !b.probeInFlight.CompareAndSwap(false, true) {
		return
	}
	shownAt := time.Now()
	go func() {
		defer b.probeInFlight.Store(false)
		time.Sleep(b.probeDelay())
		switch b.judgePage(shownAt) {
		case pageHealthy:
			return
		case pageSilent:
			// Silence is ambiguous — a wedged hub or a blocked main thread
			// recovers by itself, and replacing the window would lose the
			// user's page state for nothing. Give it one more cycle; only
			// persistent silence (or a black verdict by then) is acted on.
			time.Sleep(b.probeDelay())
			if b.judgePage(shownAt) == pageHealthy {
				return
			}
		}
		// The user closed (hid) the window while the probe slept. Replacing it
		// now would pop a fresh window onto their screen for a page they had
		// already put away — and a healthy hidden page can legitimately go
		// quiet. Nothing is lost: the next show probes again from scratch.
		if b.hidden.Load() {
			return
		}
		old := b.detachStalled(shown, time.Now())
		if old == nil {
			return
		}
		b.revive(old)
	}()
}

// revive brings up a replacement for a window whose page can't be saved. The
// new window is created BEFORE the corpse is closed: Wails' Windows backend
// posts a quit message when its window map empties (unregisterWindow, which
// does not consult ShouldQuit), and macOS/Linux quit on last-window-close
// unless the hub keeps running in the background — closing first would race
// the shell's own survival. Indirected through a field so tests can observe
// the decision without a live Wails app.
func (b *nativeBridge) revive(old *application.WebviewWindow) {
	if b.reviveFn != nil {
		b.reviveFn(old)
		return
	}
	b.showWindowAt("")
	old.Close()
}

// probeDelay is frameProbeDelay unless a test shortened it.
func (b *nativeBridge) probeDelay() time.Duration {
	if d := b.testProbeDelay; d > 0 {
		return d
	}
	return frameProbeDelay
}

// Black-window watchdog.
//
// probeAfterShow only judges a window the user just asked for. A WebView2 can
// also lose its compositor while it sits open and untouched: the window still
// drags, JS keeps beating, but every frame is stale (the reported symptom is a
// window that is there but entirely black). This loop looks for exactly that
// state and replaces the window.
//
// Every condition must hold, so the common legitimate silences never rebuild:
// visible (not hidden), recently focused (a background app or locked session
// may stop painting), JS still beating, and no frame for three minutes. A
// hidden report newer than the last frame also means no frames were owed.
const (
	blackWatchInterval = 30 * time.Second
	blackFrameStall    = 3 * time.Minute
	blackFocusWindow   = 10 * time.Minute
)

func (b *nativeBridge) watchBlackWindow() {
	b.watchOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(blackWatchInterval)
			defer ticker.Stop()
			for range ticker.C {
				b.checkBlackWindow(time.Now())
			}
		}()
	})
}

func (b *nativeBridge) checkBlackWindow(now time.Time) {
	win := b.currentWindow()
	if win == nil || b.hidden.Load() || b.allowQuit.Load() {
		return
	}
	focus := b.lastFocusBeat.Load()
	if focus == 0 || now.Sub(time.Unix(0, focus)) > blackFocusWindow {
		return
	}
	frame := b.lastFrame.Load()
	if frame == 0 || now.Sub(time.Unix(0, frame)) < blackFrameStall {
		return
	}
	if hidden := b.lastHiddenBeat.Load(); hidden > frame {
		return
	}
	if !b.reviveAllowed(now) {
		return
	}
	old := b.detachStalled(win, now)
	if old == nil {
		return
	}
	b.revive(old)
}

// Heartbeat implements server.NativeBridge: the page beats every
// heartbeatInterval (and on focus/visibilitychange) with the age of the last
// requestAnimationFrame it observed, or -1 while it is hidden and owes no
// frames. The beat proves JS alive; the frame timestamp proves the render
// pipeline alive; the hidden flag says frames aren't expected.
func (b *nativeBridge) Heartbeat(frameAgeMS int64, hidden bool, focused ...bool) {
	now := time.Now()
	b.lastBeat.Store(now.UnixNano())
	if len(focused) > 0 && focused[0] && !hidden {
		// The page is visible and holds the foreground slot. Only beats from
		// this state may drive the black-window watchdog: a window behind
		// another app, a locked session, or a sleeping display is allowed to
		// stop painting without being torn down and rebuilt behind the user's
		// back.
		b.lastFocusBeat.Store(now.UnixNano())
	}
	if hidden {
		// Not visible, so no frames are owed: record that explicitly, or the
		// probe would read the missing frame as a black window.
		b.lastHiddenBeat.Store(now.UnixNano())
		return
	}
	if frameAgeMS < 0 {
		// Visible but has never painted. Deliberately records NO frame
		// evidence — this is the black-window signature, not an excuse.
		return
	}
	age := time.Duration(frameAgeMS) * time.Millisecond
	if frameAgeMS > int64(maxReportedFrameAge/time.Millisecond) {
		age = maxReportedFrameAge
	}
	frame := now.Add(-age).UnixNano()
	// Monotonic: a stale report (a focus beat whose rAF has not ticked yet,
	// clock skew, a hostile local caller) must never erase newer evidence.
	for {
		prev := b.lastFrame.Load()
		if frame <= prev || b.lastFrame.CompareAndSwap(prev, frame) {
			return
		}
	}
}

// confirm shows a modal question dialog and reports whether the user chose the
// affirmative button. The cancel button is the safe default.
//
// The message must end in a yes/no question. Windows draws the system's own
// Yes/No buttons and takes no custom labels (see confirm_windows.go), so
// okLabel/cancelLabel are what macOS and Linux show, not a promise — a
// question those two words cannot answer ("Save" / "Discard") would read
// wrong there with nothing to catch it.
//
// Both askers reach it while the app may well not be the active one — the
// tray's "Quit Octo", clicked from whatever the user was in, and the
// launch-time takeover prompt — and a dialog that comes up behind the active
// app looks like the click did nothing. Keeping it in sight is a per-platform
// problem, so the dialog itself lives in platformConfirm.
func (b *nativeBridge) confirm(title, message, okLabel, cancelLabel string) bool {
	return platformConfirm(b.app, title, message, okLabel, cancelLabel)
}

// showError shows a modal error dialog with a single OK button.
func (b *nativeBridge) showError(title, message string) {
	dlg := b.app.Dialog.Error().SetTitle(title).SetMessage(message)
	dlg.AddButton(L().dialogOKText).SetAsDefault()
	dlg.Show()
}

// OpenExternal opens url with the OS default handler — chat links, and the
// frontend's "open in browser" actions. The server has already validated the
// scheme (http/https/mailto/tel).
// It shells out to the
// per-platform opener rather than pulling in a third-party helper; the Wails
// runtime's own browser API isn't reachable here because the page is
// octo-served, not served off Wails' asset server (same reason ExecJS is dead
// in showWindowAt).
func (b *nativeBridge) OpenExternal(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// OpenWebWindow opens url in an auxiliary in-app window instead of the system
// browser. It exists because the octo-served webview has no working
// `window.open`: Wails does not wire WebView2's NewWindowRequested, so a page
// that calls it gets nothing. The window is reused when it is still open, so
// clicking several tracks navigates one window rather than piling them up.
//
// The server has already validated the scheme (http/https).
func (b *nativeBridge) OpenWebWindow(url string) error {
	if b.app == nil {
		return fmt.Errorf("web window: app not ready")
	}
	if w := b.webWin.Load(); w != nil {
		w.SetURL(url)
		w.Show()
		w.Focus()
		return nil
	}
	w := b.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "哔哩哔哩",
		Width:  1100,
		Height: 760,
		URL:    url,
	})
	if w == nil {
		return fmt.Errorf("web window: could not create window")
	}
	b.webWin.Store(w)
	w.RegisterHook(events.Common.WindowClosing, func(*application.WindowEvent) {
		// Wails destroys the window itself once the hook returns; this only
		// clears the pointer so the next request creates a fresh one.
		b.webWin.CompareAndSwap(w, nil)
	})
	w.Show()
	w.Focus()
	return nil
}

// OpenFolder reveals a directory in the OS file manager, backing the sidebar's
// "Open folder" action. It shells out to the platform's own file-manager opener
// rather than reusing OpenExternal with a file:// URL: on Windows the URL
// protocol handler would not necessarily bring up Explorer, and `explorer` is
// the documented way to select a directory. The server has already checked that
// dir exists and is a directory.
//
// Start() rather than Run() throughout: the returned error is about spawning the
// opener, not about what it did. That also sidesteps `explorer` exiting non-zero
// on success, which a Run() here would report as a failure.
func (b *nativeBridge) OpenFolder(dir string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", dir).Start()
	case "windows":
		return exec.Command("explorer", dir).Start()
	default:
		return exec.Command("xdg-open", dir).Start()
	}
}

// confirmTakeover asks whether to stop an already-running backend and become
// the hub. Declining means the app will quit (it won't run windowed without
// its own server to attach the native bridge to).
func (b *nativeBridge) confirmTakeover(pid int) bool {
	return b.confirm(L().takeoverTitle,
		fmt.Sprintf(L().takeoverMsgFmt, pid),
		L().takeoverOK, L().takeoverCancel)
}

// requestQuit is the tray "Quit Octo" action: it fully stops the backend, so it
// confirms first when channels are running (other clients would disconnect).
func (b *nativeBridge) requestQuit() {
	if srv := b.srv.Load(); srv != nil && len(srv.RunningChannels()) > 0 {
		if !b.confirm(L().quitTitle, L().quitMsg, L().quitOK, L().quitCancel) {
			return
		}
	}
	// A real quit: let ShouldQuit (Windows/Linux) allow app termination.
	b.allowQuit.Store(true)
	b.app.Quit()
}
