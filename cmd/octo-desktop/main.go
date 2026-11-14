// Command octo-desktop is octo's native desktop shell — and, while it runs, the
// single backend every other octo interface shares. It binds the fixed loopback
// port this build owns (see internal/brand), runs the same web server in-process,
// and points a Wails window at it, so the Svelte frontend and every /api and
// /ws handler are reused unchanged and the Web UI / VS Code / Obsidian / CLI all
// connect to this one instance. On top of the server it adds the native layer a
// browser can't reach — OS folder dialog, tray, launch-at-login, notifications —
// wired in through server.NativeBridge.
//
// Only one backend owns the port at a time: the app joins the profile-scoped
// serve.pid protocol (internal/serveproc) that `octo serve -d` uses, offering to take over
// a running daemon rather than fighting for the port.
//
// See dev-docs/desktop-hub-design.md and dev-docs/wails-desktop-design.md.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/open-octo/octo-agent/internal/brand"
	"github.com/open-octo/octo-agent/internal/crashlog"
	"github.com/open-octo/octo-agent/internal/datahome"
	"github.com/open-octo/octo-agent/internal/logfile"
	"github.com/open-octo/octo-agent/internal/serveenv"
	"github.com/open-octo/octo-agent/internal/serveproc"
	"github.com/open-octo/octo-agent/internal/server"
	"github.com/open-octo/octo-agent/internal/shellpath"
	"github.com/open-octo/octo-agent/internal/version"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// Tray icons. macOS wants a monochrome template image (auto-tinted for the
// light/dark menu bar); Windows/Linux want the regular color icon. A tray with
// neither an icon nor a label is invisible on macOS, which is why one must be
// set explicitly.
//
//go:embed build/darwin/tray-icon.png
var trayTemplateIcon []byte

//go:embed build/linux/icon.png
var trayColorIcon []byte

// defaultHubAddr is the fixed loopback address the hub owns, from the single
// brand definition so two differently-branded builds of this tree never fight
// over one port. LAN exposure stays a CLI concern.
var defaultHubAddr = brand.DesktopAddr()

// hubAddr is the address this launch's profile answers on: the fixed default
// for the default profile, and the profile's own remembered port otherwise —
// the same answer `octo serve` reaches, via the same resolver, so a profile is
// at one address no matter which backend is up. Seeded from the pin in main,
// then resolved for real in startHub — which runs after the tray-refresh loop
// is already reading it, so it is atomic like the bridge's other
// startHub-written fields.
var hubAddr = func() atomic.Value {
	v := atomic.Value{}
	v.Store(defaultHubAddr)
	return v
}()

// isBundled reports whether we're running inside a .app. The Wails
// notifications service needs a bundle identifier and hard-fails startup
// without one, so it's registered only when bundled — a bare `make desktop`
// binary still runs, just without native notifications.
func isBundled() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(exe, ".app/Contents/MacOS/")
}

// notificationsAvailable reports whether the OS-native notification service can
// be created on this platform. macOS's UNUserNotificationCenter needs a bundle
// identifier, so it only works from a .app bundle; Windows (a registered COM
// toast activator) and Linux (D-Bus) work for any running executable, so the
// tray update toast reaches all three platforms uniformly.
func notificationsAvailable() bool {
	if runtime.GOOS == "darwin" {
		return isBundled()
	}
	return true
}

// fallbackDirIfRootLaunch returns the directory the hub should switch to given
// its inherited working dir and a fallback dir, or "" to keep the inherited
// dir. A GUI launcher drops a double-clicked app at a filesystem root — "/" for
// Finder/LaunchServices on macOS and several Linux .desktop launchers, a drive
// root like "C:\" on Windows — detected here as a path that is its own parent;
// an os.Getwd error surfaces as an empty wd. In those cases the fallback (the
// portable root: the exe's own directory) is the sane default. A meaningful cwd
// (a terminal launch from a project dir) is left untouched. Not covered: a
// Windows launch that inherits a system directory such as C:\Windows\System32 —
// that is not a root, so this leaves it in place.
func fallbackDirIfRootLaunch(wd, fallback string) string {
	if fallback == "" {
		return ""
	}
	if wd == "" || filepath.Dir(wd) == wd {
		return fallback
	}
	return ""
}

// ensureWorkingDir moves the process out of a root/unknown launch directory
// into the portable root — the directory holding the executable. The server's
// launch dir still seeds skill discovery and the project-memory root
// (server.go), so a Finder-launched app left at "/" would otherwise run those
// from the filesystem root — the reported bug. Portable builds keep program,
// data and workspaces together, so the exe directory is the anchor. Best
// effort: a chdir failure leaves the inherited dir in place.
func ensureWorkingDir() {
	wd, _ := os.Getwd()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	fallback := filepath.Dir(exe)
	if dir := fallbackDirIfRootLaunch(wd, fallback); dir != "" {
		_ = os.Chdir(dir)
	}
}

// ensureValidTempDir unsets $TMPDIR when it doesn't point at a usable
// directory (missing, or a file rather than a directory), so Go's
// os.TempDir() (and the Wails updater's os.MkdirTemp("", ...) on top of it)
// falls back to the platform default instead of failing outright.
func ensureValidTempDir() {
	dir := os.Getenv("TMPDIR")
	if dir == "" {
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		os.Unsetenv("TMPDIR")
	}
}

// webviewUserDataPath pins WebView2's profile (cache, localStorage) under
// <exe dir>\Data\Cache\webview so a portable copy carries it, keeps it out of
// %APPDATA%, and leaves "delete the cache" a single folder. Best-effort: on
// failure return "" and Wails keeps its default.
func webviewUserDataPath() string {
	dir, err := datahome.SharedPath(datahome.RegionCache, "webview")
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

func main() {
	if err := selectDesktopProfile(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "octo-desktop: invalid --profile: %v\n", err)
		os.Exit(2)
	}

	// Settle the data layout before anything touches it: WebView2 has to be
	// handed its profile path before it opens a single file, and the server
	// reads config/sessions while it starts. Doing this here also keeps the
	// migration out of the window where a WebView2 helper still holds the cache.
	_ = datahome.EnsureLayout()
	// The pid/log files and a WebView2 profile that is still shutting down can
	// hold a handle during the first seconds; try the cache moves again once the
	// app has settled, without blocking startup.
	go func() {
		time.Sleep(6 * time.Second)
		datahome.ReclaimStaleCache()
	}()

	// A profile switch starts us while the process we replace is still winding
	// down. Wait it out before anything observable happens — ahead of
	// application.New (which would hand us to the old instance as a second
	// launch) and ahead of any log file the old backend still holds open.
	awaitPredecessor()

	// Seed the hub address from the profile's pin — without probing — so the
	// bridge and tray built below carry the right address from the start. The
	// authoritative resolution (first-run selection, busy-pin reporting) still
	// happens in startHub, inside the event loop where its dialogs can run.
	if p := os.Getenv(datahome.ProfileEnv); p != "" {
		if pinned, ok := serveproc.ReadAddr(); ok {
			hubAddr.Store(pinned)
		}
	}

	// macOS's postinstall script launches the app with `open` from inside
	// installd's ephemeral PKInstallSandbox.*; the launched process can inherit
	// that sandbox's $TMPDIR. The desktop app then runs for days as a tray
	// resident, long after the sandbox is torn down, so a later update check's
	// os.MkdirTemp fails with "no such file or directory" against a $TMPDIR
	// that hasn't existed since install. Clear it before anything (including
	// the updater helper below) can use it.
	ensureValidTempDir()

	// Point stderr at the profile-scoped crash.log before anything that can die runs. This
	// process has no usable stderr of its own (Windows: built with -H windowsgui,
	// so no console; macOS: launched from Finder), and the runtime writes panic
	// traces straight to the descriptor — below the slog/log redirection
	// setupHubLog installs later. Without this, an unrecovered panic in any
	// goroutine closes the window and leaves nothing behind to report. Ahead of
	// the helper mode below on purpose: swapping a staged update over a running
	// install is exactly the kind of file work whose failures need a record.
	setupCrashLog()

	// A GUI launch inherits "/" as the working directory; move to the user's
	// home before anything reads it (the in-process server records its launch
	// dir as the skill-discovery and project-memory root).
	ensureWorkingDir()

	// A GUI launch also inherits a minimal PATH (macOS: no ~/.local/bin,
	// /opt/homebrew/bin; Linux: a .desktop/systemd launch may skip the login
	// profile entirely). The server runs in-process here, so stdio MCP children
	// and shell tools inherit this process's PATH directly — sync it to the login
	// shell's before server.New below, mirroring the `octo serve` binary.
	shellpath.SyncToLoginShell()

	// Load the profile-scoped serve.env for variables a GUI launch can't inherit from a
	// login shell (e.g. TAVILY_API_KEY, provider keys). Best-effort — missing
	// file is a no-op, explicit env always wins. Mirrors the `octo serve` CLI
	// path so both backends resolve the same set of environment variables.
	serveenv.Load()

	// Pick the language for native dialogs/tray from the system UI language.

	// Pick the language for native dialogs/tray from the system UI language.
	applyLang()

	settings := loadDesktopSettings()

	// Seed the shared bin/uv from the app's bundled copy on first run so skills
	// that need Python work even for a standalone download (no installer).
	ensureBundledUv()

	// Seed the octo CLI to ~/.local/bin (macOS + Linux) so a terminal has `octo`,
	// and on macOS put that dir on PATH. May update settings.SeededOctoVersion, so
	// it runs before the bridge takes its copy of settings below.
	ensureBundledOcto(&settings)

	bridge := &nativeBridge{settings: settings}
	bridge.setURL("http://" + hubAddr.Load().(string))
	// On Windows/Linux a window close would otherwise quit the app; start with
	// quit allowed only when the user opted out of keep-running-in-background.
	bridge.allowQuit.Store(!settings.KeepRunningInBackground)

	// Native notifications where the platform supports them (see
	// notificationsAvailable — macOS requires a bundle, Windows/Linux don't).
	// Registered as a Wails service so its ServiceStartup runs; the bridge holds
	// it to send notifications the frontend and the tray update check request.
	var services []application.Service
	if notificationsAvailable() {
		notifier := notifications.New()
		bridge.notifier = notifier
		services = append(services, application.NewService(notifier))
	}

	app := application.New(application.Options{
		Name:        brand.Name,
		Description: brand.Name,
		Services:    services,
		SingleInstance: &application.SingleInstanceOptions{
			// Must differ from the upstream build's ID: a shared mutex makes the
			// second process exit silently after raising the first one's window.
			UniqueID: brand.SingleInstanceID,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				bridge.showWindow()
			},
		},
		// ShouldQuit is consulted on every termination attempt. On Windows/Linux
		// that includes closing the last window, so returning allowQuit there
		// keeps the hub alive in the tray (reopen via "Show Octo" or relaunch)
		// until the user picks "Quit Octo". macOS never quits on window close
		// thanks to the option below and handles real quits (Cmd-Q) itself, so
		// it always allows the quit.
		ShouldQuit: func() bool {
			if runtime.GOOS == "darwin" {
				return true
			}
			return bridge.allowQuit.Load()
		},
		Mac: application.MacOptions{
			// Closing the window must not quit the hub when the user wants it to
			// keep serving other clients in the background; the tray keeps it
			// reachable. When they opt out, last-window-close quits as usual.
			ApplicationShouldTerminateAfterLastWindowClosed: !settings.KeepRunningInBackground,
		},
		Windows: application.WindowsOptions{
			// Keep WebView2's own profile out of %APPDATA%: the cache and the
			// UI-side localStorage mirror travel with Data\ like everything else.
			WebviewUserDataPath: webviewUserDataPath(),

			// Wails' Windows backend posts a quit message the moment its window
			// map empties (unregisterWindow), without consulting ShouldQuit —
			// so closing the window terminated the whole hub even when the user
			// asked it to keep running in the tray, and it also raced the
			// webview revive's window swap. Suppress that and let octo's own
			// ShouldQuit be the only authority, as it already is on mac/Linux;
			// the close path quits explicitly when the user opted out of
			// background running (see closeShouldQuit).
			DisableQuitOnLastWindowClosed: true,
			// Chromium's native-window occlusion tracker stops rendering a
			// WebView2 whose window is minimised or fully covered; on some
			// machines the compositor never paints again after restore,
			// leaving a permanently black window (WebView2Feedback#5171).
			// Disable the tracker so a backgrounded window keeps its render
			// pipeline alive. Wails appends this to its own disabled-feature
			// defaults. The cost is a still-rendering hidden webview, which
			// is acceptable for a tray-resident hub.
			DisabledFeatures: []string{"CalculateNativeWinOcclusion"},
		},
	})
	bridge.app = app

	// Double-clicking the pet shows the window or puts it away. Registered
	// once for the app, not per pet window: the listener outlives any
	// individual pet, and re-registering on every showPet would stack a
	// handler per appearance.
	app.Event.On(petToggleWindowEvent, func(*application.CustomEvent) {
		bridge.petToggleMainWindow()
	})

	// Interacting with a session notification focuses and routes to the
	// relevant session; every other notification just raises the window. A tap
	// on the body or the default action carries a session id in UserInfo.
	if bridge.notifier != nil {
		bridge.notifier.OnNotificationResponse(func(res notifications.NotificationResult) {
			if sid, ok := res.Response.UserInfo["session_id"].(string); ok && sid != "" {
				bridge.showWindowAt("chat/" + url.PathEscape(sid))
				return
			}
			bridge.showWindow()
		})
	}

	// macOS only: take over the menu bar so its Quit reaches requestQuit too
	// (see buildAppMenu). Elsewhere the framework's Quit already runs through
	// ShouldQuit, which honours allowQuit.
	if runtime.GOOS == "darwin" {
		app.Menu.Set(buildAppMenu(app, bridge))
	}

	// System tray: reach the window or fully quit without hunting for the dock
	// icon. Quit goes through requestQuit so it can warn when stopping the hub
	// would disconnect other clients. An icon is required — a status item with
	// neither icon nor label doesn't render on macOS.
	tray := app.SystemTray.New()
	bridge.tray.Store(tray) // let update checks refresh the menu on their own goroutine
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(trayTemplateIcon)
	} else {
		tray.SetIcon(trayColorIcon)
	}
	tray.SetTooltip(brand.Name + " · " + hubAddr.Load().(string))
	tray.SetMenu(buildTrayMenu(app, bridge))
	// Show, not toggle. Wails' own default for a tray with a window attached
	// hides a window that is already visible, which reads as "the tray click
	// did nothing" whenever the window is open but buried behind the app the
	// user is actually in — the state this click exists to get out of. Putting
	// the window back in the tray already has its own gesture: closing it.
	if trayClickOpensWindow(runtime.GOOS) {
		tray.OnClick(func() { bridge.showWindow() })
	}
	// Keep the tray's status lines (backend, channels, connected clients) fresh
	// while the app runs — macOS doesn't refresh a status menu on open.
	go refreshTrayLoop(app, tray, bridge)

	// Bind + serve + open the window once the event loop is up, so the takeover
	// prompt (a modal dialog) can run. Doing it here rather than before Run lets
	// us ask the user before stopping someone else's backend.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		// Prompt for notification permission (macOS blocks until answered, so
		// off the UI thread) — without it every toast silently no-ops.
		go bridge.requestNotificationAuthorization()
		// The server is in-process: a restart means "start this exe again".
		// relaunchSelf() already starts a copy that waits for this pid to exit.
		server.SetRestartHook(func() {
			if err := relaunchSelf(); err != nil {
				bridge.showError(L().errTitle, err.Error())
				return
			}
			bridge.allowQuit.Store(true)
			app.Quit()
		})
		startHub(app, bridge, settings)
	})

	// macOS: clicking the dock icon after the window was closed (hidden to the
	// tray) fires "reopen" — re-create/show the window instead of no-op'ing.
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		bridge.showWindow()
	})

	err := app.Run()

	// The app has quit: release our pid-file entry (only if it's still ours —
	// a successor that took the port over must keep its own) and shut the
	// server down cleanly.
	serveproc.ReleaseOwned(os.Getpid())
	if srv := bridge.srv.Load(); srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	if closeLog := bridge.closeLog.Load(); closeLog != nil {
		(*closeLog)()
	}
	if err != nil {
		log.Fatalf("octo-desktop: %v", err)
	}
}

// setupCrashLog redirects the process's stderr to the profile-scoped crash.log so a panic
// that kills the app leaves a trace behind. Best-effort: if it fails there is
// nowhere to report that failure to (that being the whole problem), so the app
// starts anyway.
func setupCrashLog() {
	// Started from a terminal (a developer running the binary directly): that
	// terminal is a better place for a crash than a file nobody is tailing, and
	// taking stderr away would leave them staring at a silent window. The
	// shipped app never gets here — a -H windowsgui build has no console even
	// when launched from one, and a Finder/launchd launch has no terminal.
	if isatty.IsTerminal(os.Stderr.Fd()) {
		return
	}
	path, err := serveproc.CrashLogPath()
	if err != nil {
		return
	}
	banner := fmt.Sprintf("octo-desktop %s (%s/%s)", version.Version, runtime.GOOS, runtime.GOARCH)
	_ = crashlog.Install(path, banner)
}

// setupHubLog routes slog and the stdlib logger to a self-rotating
// profile-scoped serve.log and returns a close func (nil if setup failed, leaving the
// default stderr in place). The stdlib logger is redirected too so the channel
// adapters' error/retry lines — still on `log` — land in the same file.
func setupHubLog() func() {
	logPath, err := serveproc.LogPath()
	if err != nil {
		return nil
	}
	lw, err := logfile.Open(logPath, logfile.DefaultMaxBytes, logfile.DefaultBackups)
	if err != nil {
		return nil
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(lw, &slog.HandlerOptions{Level: hubLogLevel()})))
	log.SetOutput(lw)
	return func() { _ = lw.Close() }
}

// hubLogLevel reads OCTO_LOG_LEVEL (debug|info|warn|error), defaulting to info —
// matching `octo serve`'s level handling so the two backends behave alike.
func hubLogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OCTO_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// startHub takes ownership of the loopback port (offering to take over a running
// daemon), starts the in-process server, and opens the window. It runs inside
// the ApplicationStarted hook so its dialogs have a live event loop.
func startHub(app *application.App, bridge *nativeBridge, settings desktopSettings) {
	// If another backend already owns this profile, ask before displacing it —
	// first, because ResolveAddr probes the pinned port, and the backend we're
	// about to replace is exactly what would be holding it. Resolving first
	// would turn the takeover prompt into a hard "address in use" error for
	// every named profile.
	tookOver := false
	if pid, ok := serveproc.Running(); ok {
		if !bridge.confirmTakeover(pid) {
			app.Quit()
			return
		}
		if _, err := serveproc.Stop(); err != nil {
			bridge.showError(L().errTitle, fmt.Sprintf(L().errStopFmt, err))
			app.Quit()
			return
		}
		tookOver = true
	}

	// Which port this profile answers on. The default profile keeps 8088; a
	// named one gets its own remembered port, resolved the same way `octo
	// serve` resolves it, so a profile is at one address either way. A busy
	// pinned port is reported rather than worked around — see ResolveAddr.
	addr := defaultHubAddr
	if profile := os.Getenv(datahome.ProfileEnv); profile != "" {
		if pinned, ok := serveproc.ReadAddr(); ok && tookOver {
			// The port we just freed may still be draining — trust the pin and
			// let listenHub's grace period wait it out, rather than probing a
			// port the outgoing backend might still hold and failing on ourselves.
			addr = pinned
		} else {
			resolved, err := serveproc.ResolveAddr(profile, false, defaultHubAddr)
			if err != nil {
				bridge.showError(L().errTitle, err.Error())
				app.Quit()
				return
			}
			addr = resolved
		}
	}
	hubAddr.Store(addr)
	bridge.setURL("http://" + addr)

	// After a takeover, the stopped daemon needs a moment to release the port —
	// serveproc.Stop only signals it. Retry the bind for a few seconds so the
	// handoff is seamless; a cold start with a genuine conflict fails at once.
	grace := time.Duration(0)
	if tookOver {
		grace = 8 * time.Second
	}
	ln, err := listenHub(addr, grace)
	if err != nil {
		bridge.showError(L().errTitle, fmt.Sprintf(L().errBindFmt, addr, err))
		app.Quit()
		return
	}
	if path, perr := serveproc.PidPath(); perr == nil {
		_ = serveproc.WritePid(path, os.Getpid())
	}

	// Only now, having taken over any prior daemon and bound the port, are we the
	// sole backend — so it's safe to open the shared profile-scoped serve.log. A prior
	// `octo serve -d` that was stopped above has since exited (listenHub only
	// succeeds once the port is free), releasing the fd it held on the file;
	// opening/rotating earlier (e.g. in main, before the takeover) could rotate a
	// file a live daemon still holds open — on Windows the rename would fail
	// outright and drop us to a console for the whole session. Set up before
	// server.New so the hub's own startup logs are captured too.
	if closeLog := setupHubLog(); closeLog != nil {
		bridge.closeLog.Store(&closeLog)
	}

	// Also now-we're-the-sole-backend: age out old upload/attachment files
	// (#2004). The desktop hub never goes through cmd/octo's runServe, so
	// without this call here it would silently never run — this IS the
	// long-running "desktop hub" instance the housekeeping is for.
	server.StartUploadsHousekeeping()

	srv, err := server.New(server.Config{
		// Served via ServeOn(ln) below, so this is not a bind address — it
		// tells sessions which port the REST API is on.
		Addr:   ln.Addr().String(),
		Tools:  true,
		Native: bridge,
		// The desktop server runs in-process — there is no supervisor to
		// respawn it after a restart, so the restart_server tool would just
		// shut the HTTP server down and leave the GUI window attached to a
		// dead backend. Omit the tool.
		DisableRestart: true,
	})
	if err != nil {
		bridge.showError(L().errTitle, fmt.Sprintf(L().errStartFmt, err))
		app.Quit()
		return
	}
	bridge.srv.Store(srv)
	go func() {
		if err := srv.ServeOn(ln); err != nil {
			log.Printf("octo-desktop: server stopped: %v", err)
		}
	}()

	bridge.showWindow()
}

// listenHub binds addr, retrying for up to grace so a just-stopped daemon has
// time to release the port (SIGTERM only signals it; the listener closes a
// beat later). grace of 0 means a single attempt.
func listenHub(addr string, grace time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(grace)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// trayStatusLines is the (info-only) top of the tray menu: what the hub is
// doing right now — where it's serving and how many clients are attached. The
// profile row goes under the first of these but is not one of them: it is a
// submenu, not a line (see addProfileRow), so the refresh signature carries
// the on-disk profile list separately (see trayMenuSignature).
func trayStatusLines(bridge *nativeBridge) []string {
	srv := bridge.srv.Load()
	if srv == nil {
		return []string{L().trayStarting}
	}
	lines := []string{fmt.Sprintf(L().trayBackendFmt, hubAddr.Load().(string))}
	lines = append(lines, fmt.Sprintf(L().trayClientsFmt, srv.ConnectedClients()))
	// Only when > 0 — keeps the menu clean before any channel is configured.
	if n := srv.ConfiguredChannelCount(); n > 0 {
		lines = append(lines, fmt.Sprintf(L().trayChannelsFmt, n))
	}
	return lines
}

// buildAppMenu is the macOS menu bar. It exists for one item: "Quit Octo" (and
// its Cmd-Q) has to reach requestQuit like the tray's does, so both warn before
// stopping a hub other clients are connected to. The framework's Quit carries
// AppKit's terminate: selector, which never runs a Go callback, so the item has
// to be an ordinary one. Everything else is the default menu, role for role.
//
// Doing it in ShouldQuit instead would also cover the Dock's Quit and a system
// logout, and it can't: AppKit calls that hook on the main thread
// (applicationShouldTerminate:), while the confirmation blocks waiting for the
// main thread to show the dialog. Those two routes still quit unconfirmed.
func buildAppMenu(app *application.App, bridge *nativeBridge) *application.Menu {
	menu := app.NewMenu()
	appMenu := menu.AddSubmenu(brand.Name)
	appMenu.AddRole(application.About)
	appMenu.AddSeparator()
	appMenu.AddRole(application.ServicesMenu)
	appMenu.AddSeparator()
	appMenu.AddRole(application.Hide)
	appMenu.AddRole(application.HideOthers)
	appMenu.AddRole(application.UnHide)
	appMenu.AddSeparator()
	appMenu.Add(L().trayQuit).SetAccelerator("CmdOrCtrl+Q").
		OnClick(func(*application.Context) { bridge.requestQuit() })
	menu.AddRole(application.FileMenu)
	menu.AddRole(application.EditMenu)
	menu.AddRole(application.ViewMenu)
	menu.AddRole(application.WindowMenu)
	menu.AddRole(application.HelpMenu)
	return menu
}

// buildTrayMenu assembles the tray menu: disabled status lines on top, then the
// Show/Quit actions. Rebuilt (not mutated in place) so a refresh is one
// SetMenu call, which Wails marshals to the UI thread.
func buildTrayMenu(app *application.App, bridge *nativeBridge) *application.Menu {
	m := app.NewMenu()
	lines := trayStatusLines(bridge)
	m.Add(lines[0]).SetEnabled(false)
	addProfileRow(m, bridge)
	for _, line := range lines[1:] {
		m.Add(line).SetEnabled(false)
	}
	m.AddSeparator()
	m.Add(L().trayShow).OnClick(func(*application.Context) { bridge.showWindow() })
	m.Add(L().trayNewSession).OnClick(func(*application.Context) { bridge.openNewSession() })
	// The pet item is a toggle, so it names what the click will do — the menu is
	// rebuilt on every toggle (see togglePet), not only on refreshTrayLoop's tick.
	petLabel := L().trayPet
	if bridge.petShown() {
		petLabel = L().trayPetHide
	}
	m.Add(petLabel).OnClick(func(*application.Context) { bridge.togglePet() })
	m.Add(L().traySettings).OnClick(func(*application.Context) { bridge.openSettings() })
	m.AddSeparator()
	m.Add(L().trayQuit).OnClick(func(*application.Context) { bridge.requestQuit() })
	return m
}

// trayMenuSignature is what refreshTrayLoop compares between ticks: the menu
// is rebuilt only when this changes. It carries the status lines and the
// on-disk profile list — the profile row's title is fixed for the process, but
// its submenu lists data roots that the Web UI's data-management panel or
// `octo profiles` can add/remove at any time, so without this a deleted
// profile lingers in the menu until restart. The status lines are
// language-dependent, so a language switch (re-applied here) changes the
// signature and triggers a rebuild that re-reads L(). The pet is in because its
// row names what the next click does, and the refreshTray that follows a pet
// appearing or going away could race a rebuild that read the state just before
// it changed.
func trayMenuSignature(bridge *nativeBridge) string {
	applyLang() // follow a language switch made in onboarding / Settings
	pet := ""
	if bridge.petShown() {
		pet = "pet"
	}
	profiles, err := datahome.List()
	if err != nil {
		profiles = nil // an unreadable home must not wedge the refresh loop
	}
	return strings.Join(trayStatusLines(bridge), "|") + "\x00" + pet +
		"\x00" + strings.Join(profiles, "|")
}

// refreshTrayLoop re-publishes the tray menu whenever its status text changes,
// so the counts stay live without rebuilding on every tick.
func refreshTrayLoop(app *application.App, tray *application.SystemTray, bridge *nativeBridge) {
	last := trayMenuSignature(bridge)
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		sig := trayMenuSignature(bridge)
		if sig == last {
			continue
		}
		last = sig
		tray.SetMenu(buildTrayMenu(app, bridge))
	}
}
