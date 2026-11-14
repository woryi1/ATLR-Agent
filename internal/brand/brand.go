// Package brand is the single source of truth for the product identity of this
// build: the window/application name, the OS-level application ID (single
// instance mutex, Windows AppUserModelID, macOS bundle identifier), the
// desktop hub's default address, the artefact name, and whether this build
// talks to the upstream release feed at all.
//
// Every field is a package-level var on purpose: -ldflags -X can rewrite a
// string var at link time but not a const, and this repository is built as two
// products from one tree (upstream Octo and ATLR-Agent). The defaults below are
// the ATLR-Agent values; scripts/build-desktop-windows.ps1 injects them again
// explicitly and then asserts the injected identity is present in the finished
// exe, because a missed injection is silent — the binary just falls back to the
// defaults while the two builds' mutexes and ports collide again.
//
// UpstreamUpdates is a string rather than a bool for the same -X reason; read it
// through UpstreamUpdatesEnabled.
package brand

// Name is the user-visible product name: window title, tray tooltip, dialog
// titles, macOS application menu.
var Name = "ATLR-Agent"

// AppID is the OS-level application identity. On Windows it is the
// AppUserModelID used by toasts and taskbar grouping; on macOS it is
// CFBundleIdentifier. It must differ from the upstream Octo app's ID or the two
// installs share toast registrations and LaunchServices treats them as one app.
var AppID = "dev.atlr.agent.desktop"

// SingleInstanceID scopes the process-wide single-instance mutex. Two builds
// with the same value cannot run at the same time: the second process exits
// silently after raising the first one's window. Changing it breaks
// coexistence with the old build — that is exactly what it is for here.
var SingleInstanceID = "dev.atlr.agent.desktop"

// DesktopPort is the loopback port the desktop hub binds. The CLI (`octo
// serve`) keeps its own 8088 default: the CLI is not the delivered artefact
// here, and moving it would break every existing client.
var DesktopPort = "9088"

// UpstreamUpdates is retained for build-script compatibility (the packaging
// script still injects it) but has no consumers in this tree any more: the
// update/version-check code was removed outright rather than gated off. Kept as
// a string rather than a bool for the same -X reason.
var UpstreamUpdates = "false"

// ExeName is the desktop executable's file name, asserted against the file the
// build script actually produced.
var ExeName = "ATLR-Agent.exe"

// DesktopAddr returns the loopback address the desktop hub binds.
func DesktopAddr() string { return "127.0.0.1:" + DesktopPort }

// UpstreamUpdatesEnabled reports whether this build may talk to the upstream
// release feed. No code consults it now that the update channel is gone; it is
// kept alongside UpstreamUpdates so the packaging script's injected symbol
// still resolves.
func UpstreamUpdatesEnabled() bool { return UpstreamUpdates == "true" }

// PortableZipName is the portable archive name for a version, e.g.
// ATLR-Agent-0.5-portable.zip.
func PortableZipName(version string) string { return Name + "-" + version + "-portable.zip" }
