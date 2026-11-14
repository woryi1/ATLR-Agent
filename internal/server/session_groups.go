package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/open-octo/octo-agent/internal/agent"
	"github.com/open-octo/octo-agent/internal/datahome"
	"github.com/open-octo/octo-agent/internal/memory"
)

// The sidebar has exactly two concepts the user can create: a task (one loose
// session) and a project (a named directory plus the sessions working in it).
// This registry is where projects live, and it also holds the one group the
// system makes on its own: a cron task's per-run cluster. There is no third
// user-facing concept — a "plain group", a name with sessions under it and no
// directory, used to exist and no longer does: it split the sidebar into three
// kinds of row for two kinds of thing, and offered organisation that carried
// none of what a project carries (a directory for the tools, a memory tier).
// dissolvePlainGroups retires the ones already on disk.
//
// The registry lives entirely in one file
// (~/.octo/session-groups.json) and never touches the session transcript format
// — group membership is stored here as group→session-ID lists, and no session
// field is added. The CLI reads it (a session in a project belongs to the
// project's directory, which is what `octo -c` scopes its listing by) and
// writes it (EnsureProjectForDir), so the file is shared between processes
// rather than owned by the server — see registryLock. A session belongs to at
// most one group; a session ID that no longer resolves to a real
// transcript is simply not rendered (the frontend cross-references the live
// session list), so stale IDs left by a deleted session are harmless. There is
// no GC pass for them — a stale ID is only dropped when its group is next
// rewritten (a move touching that group, or the frontend re-render filtering
// it out); the list grows at most by one dead entry per deleted session, which
// is negligible.
//
// The desktop app (cmd/octo-desktop) runs this same server in-process against
// the same ~/.octo, so groups and their collapsed state are shared between the
// Web UI and the desktop shell with no extra wiring.

// sessionGroup is one project in the registry: a working directory plus the
// sessions working in it. WorkingDir being set is what makes it one, and a group
// without one is a plain group — a concept that no longer exists, dissolved at
// startup.
//
// A scheduled task's runs live in a project too, created by the scheduler and
// named after the task, working in <workspace>/<task name>. TaskID records which
// task that was: it is what lets the startup pass repair such a project instead
// of dissolving it, and it survives a scheduler that failed to start. It carries
// no UI or permission meaning — the project behaves like any other, because it is
// one.
type sessionGroup struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SessionIDs []string `json:"session_ids"`
	// Collapsed is persisted server-side (rather than in browser localStorage)
	// so the folded/expanded state survives across reloads and is identical in
	// the Web UI and the Wails desktop webview, whose local storage may not
	// persist the same way.
	Collapsed bool `json:"collapsed,omitempty"`
	// WorkingDir, when set, makes this group a project (see above). It is the
	// project's octo-owned workspace, generated under the workspace root at
	// creation and fixed for the project's life. Stored already expanded and
	// absolute — the write paths resolve and validate it, so every reader can
	// use it verbatim. (Registries written before the workspace model may
	// still carry a user directory here; the startup migration converts them.)
	WorkingDir string `json:"working_dir,omitempty"`
	// SourceDirs are the external directories the project references — code
	// repositories and plain folders alike, mounted as additional roots for
	// the tools. A directory may appear in several projects' SourceDirs; it
	// may never lie under the workspace root (the write paths reject that).
	SourceDirs []string `json:"source_dirs,omitempty"`
	// TaskID, when set, is the scheduled task this project was created for. It
	// is what lets the startup pass repair such a project (backfilling the
	// directory a task written before this had none) rather than dissolving it.
	TaskID string `json:"task_id,omitempty"`
	// ReasoningEffort is the project's own reasoning intensity — the level a
	// NEW session born here starts on, and the fallback an older session
	// without a value of its own resolves against. "off" is stored literally:
	// it is an explicit choice that must outrank the global default. Empty
	// means "never set" (every registry written before this field), which keeps
	// resolving against the global config exactly as it did.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// SuperMode is the project's own Super-mode switch: when true, every
	// system prompt composed for a session in this project carries the
	// supermode.md preamble.
	//
	// A POINTER, so "this project never chose" (nil) stays distinguishable
	// from "this project chose off" (false). With a plain bool the two
	// collapsed into false, and every project then silently outranked the
	// global config.yml switch — a global toggle could never reach a
	// session that belonged to a project, which is most of them. nil now
	// resolves against the global value (inherit); false is an explicit
	// "off" that outranks it. A registry written before this field decodes
	// to nil without an error.
	SuperMode *bool `json:"super_mode,omitempty"`
	// LastSessionID is the session the user was last looking at in this
	// project. Entering the project opens it again; "" (the zero value for
	// every registry written before this field) means "no memory yet", and
	// the caller falls back to the project's most recent session.
	LastSessionID string `json:"last_session_id,omitempty"`
}

// isProject reports whether this group carries project settings.
func (g sessionGroup) isProject() bool { return g.WorkingDir != "" }

// isCronCluster reports whether this project was created for a scheduled task.
// Used by the startup repair pass, not to gate anything the user can do.
func (g sessionGroup) isCronCluster() bool { return g.TaskID != "" }

// groupFile is the on-disk shape of the registry. Group order is array order.
// PinnedSessionIDs is the Web-UI "pinned" set — sessions the user floated to a
// dedicated section at the top of the sidebar. It lives in this same file (the
// one web-only session-organisation layer) rather than on the session, so the
// CLI/TUI listing is unaffected. Array order is pin order; a pinned ID that no
// longer resolves to a live session is simply not rendered.
type groupFile struct {
	Groups           []sessionGroup `json:"groups"`
	PinnedSessionIDs []string       `json:"pinned_session_ids,omitempty"`
	// CollapsedSessionIDs is the Web-UI "collapsed" set — sessions the user
	// explicitly tucked into a folded panel at the bottom of the sidebar to keep
	// the main list short. Mutually exclusive with pinning and group membership
	// (a collapsed session belongs to neither); the write handlers enforce that.
	// Array order is collapse order; an ID that no longer resolves to a live
	// session is simply not rendered.
	CollapsedSessionIDs []string `json:"collapsed_session_ids,omitempty"`
}

// notifyGroupsChanged, when set (Server.New), is called after every successful
// registry write so open Web/desktop tabs can refetch the groups snapshot —
// without it, a project's working directory changed in one tab stays stale in
// every other until reload, and a stale project directory misleads about
// where a session's tools run. One hook at the single write point
// (saveRegistry) rather than a broadcast per HTTP handler: the programmatic
// writers (cron's group helpers, create-session-in-group) are covered too,
// and no future write path can forget it. Called under groupMu — the hub's
// buffered events channel makes that non-blocking in practice, same as every
// other broadcast issued under a lock.
//
// Package-level like groupMu/regCache: the registry is per-~/.octo process
// state, and the last Server to start owns the notification. A no-op when nil
// (tests that never construct a Server).
var notifyGroupsChanged func()

// groupMu serialises access to the registry (regCache included). Lock for a
// read; LockWrite for a read-modify-write cycle, which additionally takes a
// cross-process lock now that the CLI writes the registry too. Both pair with
// a plain Unlock. See registryLock for why reads stay out of the file lock.
var groupMu = &registryLock{}

// sessionGroupsPath returns the project registry, creating its folder.
func sessionGroupsPath() (string, error) {
	path, err := datahome.Path("session-groups.json")
	if err != nil {
		return "", fmt.Errorf("session groups: home dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("session groups: mkdir %s: %w", filepath.Dir(path), err)
	}
	return path, nil
}

// loadRegistryFile reads and parses the whole registry file. A missing file is
// not an error — it means an empty registry. The field-scoped load/save
// helpers below all go through it, so a save of one field can never clobber
// the others. Caller should hold groupMu for read-modify-write cycles.
func loadRegistryFile() (groupFile, error) {
	var gf groupFile
	path, err := sessionGroupsPath()
	if err != nil {
		return gf, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return gf, nil
		}
		return gf, fmt.Errorf("session groups: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &gf); err != nil {
		return gf, fmt.Errorf("session groups: parse %s: %w", path, err)
	}
	return gf, nil
}

// loadSessionGroups reads the registry. A missing file is not an error — it
// means no groups yet. Caller should hold groupMu for read-modify-write cycles.
func loadSessionGroups() ([]sessionGroup, error) {
	gf, err := loadRegistryFile()
	if err != nil {
		return nil, err
	}
	return gf.Groups, nil
}

// saveRegistry writes the whole registry (groups + pins) atomically (temp file
// + rename), the same pattern the scheduler uses. Caller must hold groupMu.
func saveRegistry(gf groupFile) error {
	if gf.Groups == nil {
		gf.Groups = []sessionGroup{}
	}
	path, err := sessionGroupsPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(gf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + "." + strconv.FormatInt(time.Now().UnixNano(), 10) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := renameWithRetry(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session groups: write %s: %w", path, err)
	}
	invalidateRegistryCache()
	if notifyGroupsChanged != nil {
		notifyGroupsChanged()
	}
	return nil
}

// renameWithRetry replaces newpath with oldpath, retrying briefly when the
// destination is momentarily open elsewhere. Reads of the registry deliberately
// stay outside the cross-process file lock (see registryLock), so another
// process can be mid-ReadFile on session-groups.json while this one renames
// over it. That is harmless on POSIX, but Go opens files on Windows without
// FILE_SHARE_DELETE, so MoveFileEx onto an open file fails with
// ERROR_ACCESS_DENIED until the reader closes — microseconds later. Seen as
// "rename ...: Access is denied." from TestEnsureProject_CrossProcessLock…
// on windows-latest. Same shape as the scheduler's helper of the same name.
func renameWithRetry(oldpath, newpath string) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// saveSessionGroups persists the group list while preserving every other list
// sharing the same file (pins, collapsed) — a group edit must never clobber
// them. Caller must hold groupMu.
func saveSessionGroups(groups []sessionGroup) error {
	gf, err := loadRegistryFile()
	if err != nil {
		return err
	}
	gf.Groups = groups
	return saveRegistry(gf)
}

// loadPinnedSessions reads the pinned-session list from the registry. A missing
// file means nothing is pinned. Caller should hold groupMu for
// read-modify-write cycles.
func loadPinnedSessions() ([]string, error) {
	gf, err := loadRegistryFile()
	if err != nil {
		return nil, err
	}
	return gf.PinnedSessionIDs, nil
}

// loadCollapsedSessions reads the collapsed-session list from the registry. A
// missing file means nothing is collapsed. Caller should hold groupMu for
// read-modify-write cycles.
func loadCollapsedSessions() ([]string, error) {
	gf, err := loadRegistryFile()
	if err != nil {
		return nil, err
	}
	return gf.CollapsedSessionIDs, nil
}

// ─── Read-side cache + session→project reverse index ────────────────────────
//
// Resolving a session's working directory now has to answer "which project
// owns this session?", and the session-list endpoint asks that once per
// session. Re-reading and re-parsing session-groups.json N times per request
// would be silly, so reads go through a small cache holding the parsed
// registry plus a sessionID→project index built alongside it.
//
// Invalidation is belt-and-braces: saveRegistry drops the cache outright
// (exact for this process's own writes), and every load re-stats the file so a
// write by another process serving the same ~/.octo (the desktop shell) is
// picked up too. Callers must treat the returned data as READ-ONLY — it is the
// cached copy, not a clone. The read-modify-write paths deliberately keep
// using loadSessionGroups, which always re-reads from disk and hands back
// data they may mutate freely.

type registryCache struct {
	// path is part of the key, not just modTime/size: HOME is redirected
	// per-test, and without it a cache entry from one temp dir could be served
	// for another whose file happens to match in size and timestamp.
	path     string
	modTime  time.Time
	size     int64
	file     groupFile
	projects map[string]*sessionGroup // session ID → the project owning it
}

// regCache is guarded by groupMu, like every other access to the registry.
var regCache *registryCache

// invalidateRegistryCache drops the cached registry. Caller must hold groupMu.
func invalidateRegistryCache() { regCache = nil }

// cachedRegistry returns the parsed registry and the session→project index,
// re-reading the file only when it has changed on disk. The returned values
// are read-only. Caller must hold groupMu.
func cachedRegistry() (groupFile, map[string]*sessionGroup, error) {
	path, err := sessionGroupsPath()
	if err != nil {
		return groupFile{}, nil, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// No registry yet — nothing is grouped. Cache nothing; a stat per
			// call is already the cheap path.
			return groupFile{}, nil, nil
		}
		return groupFile{}, nil, fmt.Errorf("session groups: stat %s: %w", path, statErr)
	}
	// modTime+size is how a write by ANOTHER process is noticed — the CLI writes
	// this file too, and its saveRegistry cannot invalidate a cache in this
	// process. The pair can theoretically miss one: a same-size rewrite landing
	// inside the filesystem's mtime granularity would read as unchanged. It
	// needs a same-byte-count edit within the same tick, and the window closes
	// on the next write; a coarser identity (hashing the file) would pay a read
	// on every lookup to close a gap nothing has hit.
	if regCache != nil && regCache.path == path && regCache.modTime.Equal(info.ModTime()) && regCache.size == info.Size() {
		return regCache.file, regCache.projects, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return groupFile{}, nil, fmt.Errorf("session groups: read %s: %w", path, err)
	}
	var gf groupFile
	if err := json.Unmarshal(data, &gf); err != nil {
		return groupFile{}, nil, fmt.Errorf("session groups: parse %s: %w", path, err)
	}
	projects := make(map[string]*sessionGroup)
	for i := range gf.Groups {
		g := &gf.Groups[i]
		if !g.isProject() {
			continue
		}
		for _, sid := range g.SessionIDs {
			projects[sid] = g
		}
	}
	regCache = &registryCache{path: path, modTime: info.ModTime(), size: info.Size(), file: gf, projects: projects}
	return gf, projects, nil
}

// projectForSession returns the project owning sessionID, or nil when the
// session is ungrouped or its group is a plain group. The result is read-only.
// Registry errors resolve to "no project" rather than propagating: a session's
// working directory must still resolve (to its own dir, or the server default)
// when the group file is unreadable.
func projectForSession(sessionID string) *sessionGroup {
	if sessionID == "" {
		return nil
	}
	groupMu.Lock()
	defer groupMu.Unlock()
	_, projects, err := cachedRegistry()
	if err != nil {
		slog.Warn("resolve project for session", "session", sessionID, "err", err)
		return nil
	}
	return projects[sessionID]
}

// canonicalReasoningLevel validates a level from the wire or a registry file,
// lowercased, and answers "" for anything unknown (including an absent key).
// "off" is preserved as "off" — a project's saved "off" is an EXPLICIT choice
// that must outrank the global default, so it cannot collapse to the same ""
// a registry written before the field uses for "never set".
func canonicalReasoningLevel(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off":
		return "off"
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		return "max"
	}
	return ""
}

// normalizeReasoningEffort is canonicalReasoningLevel in the transcript/config
// form, where "" rather than "off" spells "no reasoning effort" (the provider
// layer forwards that as an absent field).
func normalizeReasoningEffort(raw string) string {
	if canonicalReasoningLevel(raw) == "off" {
		return ""
	}
	return canonicalReasoningLevel(raw)
}

// initialReasoningEffort resolves the level a brand-new session starts on: the
// project's saved value, else the global default, else off. Pure so the
// precedence is testable without a server; the caller supplies the project's
// stored value ("" for an ungrouped session) and config.ReasoningEffort.
func initialReasoningEffort(projectValue, globalValue string) string {
	if v := canonicalReasoningLevel(projectValue); v != "" {
		return v
	}
	if v := canonicalReasoningLevel(globalValue); v != "" {
		return v
	}
	return "off"
}

// projectReasoningEffort returns the level saved on project groupID, or "".
// Read-only; a missing group or one without the field both answer "".
func projectReasoningEffort(groupID string) string {
	if groupID == "" {
		return ""
	}
	groupMu.Lock()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		slog.Warn("resolve project reasoning effort", "group", groupID, "err", err)
		return ""
	}
	for _, g := range groups {
		if g.ID == groupID {
			return canonicalReasoningLevel(g.ReasoningEffort)
		}
	}
	return ""
}

// setProjectReasoningEffort persists level on project groupID. A missing group
// is a no-op: the session may have been unsaved-long enough for its project to
// be deleted, and that must not fail the session-side write.
func setProjectReasoningEffort(groupID, level string) error {
	if groupID == "" {
		return nil
	}
	level = canonicalReasoningLevel(level)
	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		return err
	}
	for i := range groups {
		if groups[i].ID == groupID {
			if groups[i].ReasoningEffort == level {
				return nil
			}
			groups[i].ReasoningEffort = level
			return saveSessionGroups(groups)
		}
	}
	return nil
}

// projectSuperMode returns the Super-mode switch saved on project groupID.
// ok is false when groupID names no group, when the registry cannot be read,
// or when the group never made a choice of its own (SuperMode == nil) — the
// caller then falls back to the global config value. A group that explicitly
// turned it off answers (false, true): that is a deliberate choice, not an
// absence.
func projectSuperMode(groupID string) (on, ok bool) {
	if groupID == "" {
		return false, false
	}
	groupMu.Lock()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		slog.Warn("resolve project super mode", "group", groupID, "err", err)
		return false, false
	}
	for _, g := range groups {
		if g.ID == groupID {
			if g.SuperMode == nil {
				return false, false
			}
			return *g.SuperMode, true
		}
	}
	return false, false
}

// setProjectSuperMode persists on as project groupID's Super-mode switch. A
// missing group reports false so the HTTP layer can answer 404 instead of
// pretending the write landed.
func setProjectSuperMode(groupID string, on bool) (bool, error) {
	if groupID == "" {
		return false, nil
	}
	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		return false, err
	}
	for i := range groups {
		if groups[i].ID == groupID {
			if groups[i].SuperMode != nil && *groups[i].SuperMode == on {
				return true, nil
			}
			v := on
			groups[i].SuperMode = &v
			return true, saveSessionGroups(groups)
		}
	}
	return false, nil
}

// superModeForProject resolves the switch a turn composes against: the
// project's own choice when it made one, the global config value otherwise
// (a project that never chose, a loose task, or a session whose group was
// deleted). Pure so the precedence is testable without a server.
func superModeForProject(proj *sessionGroup, global bool) bool {
	if proj == nil || proj.SuperMode == nil {
		return global
	}
	return *proj.SuperMode
}

// setProjectLastSession records sessionID as the project's "where the user
// was". Membership is checked here rather than trusted from the caller: a
// session moved out between the click and this write must not become the
// project's memory.
func setProjectLastSession(groupID, sessionID string) error {
	if groupID == "" || sessionID == "" {
		return nil
	}
	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		return err
	}
	for i := range groups {
		if groups[i].ID != groupID {
			continue
		}
		member := false
		for _, id := range groups[i].SessionIDs {
			if id == sessionID {
				member = true
				break
			}
		}
		if !member {
			return fmt.Errorf("session %q is not in project %q", sessionID, groupID)
		}
		if groups[i].LastSessionID == sessionID {
			return nil
		}
		groups[i].LastSessionID = sessionID
		return saveSessionGroups(groups)
	}
	return fmt.Errorf("session group %q not found", groupID)
}

// projectDirByGroupID returns the working directory of the project with this
// group id, or "" when no group has that id or the group is not a project
// (no working dir). Read-only, like projectForSession.
func projectDirByGroupID(id string) string {
	if id == "" {
		return ""
	}
	groupMu.Lock()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		slog.Warn("resolve project dir by group", "group", id, "err", err)
		return ""
	}
	for _, g := range groups {
		if g.ID == id {
			return g.WorkingDir
		}
	}
	return ""
}

// projectSourceDirByGroupID returns the project's OWN recorded spelling of the
// mounted folder the caller named, or "" when the group is unknown or does not
// mount it. Matching is by membership, not by index, so a concurrent reorder of
// the mount list can only fail to match — never resolve to a different folder
// than the one the caller pointed at.
//
// Returning the registry's copy rather than the caller's string is the whole
// point: it is what keeps a caller-supplied path away from anything that
// consumes a path downstream (see handleNativeOpenFolder).
func projectSourceDirByGroupID(id, dir string) string {
	if id == "" || dir == "" {
		return ""
	}
	groupMu.Lock()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		slog.Warn("resolve project source dir by group", "group", id, "err", err)
		return ""
	}
	norm := memory.NormalizeDir(dir)
	for _, g := range groups {
		if g.ID != id {
			continue
		}
		for _, sd := range g.SourceDirs {
			if memory.NormalizeDir(sd) == norm {
				return sd
			}
		}
		return ""
	}
	return ""
}

// ProjectDirForSession returns the working directory of the project owning
// sessionID, or "" when the session is not in one. Exported for the CLI/TUI,
// which have no server to ask: a session filed under a project should run in
// the project's directory there too, wherever octo happened to be launched
// from. Read-only by design — the CLI offers no way to change a working
// directory, so a project's setting can only be edited where it was made.
func ProjectDirForSession(sessionID string) string {
	if p := projectForSession(sessionID); p != nil {
		return p.WorkingDir
	}
	return ""
}

// ProjectExistsForDir reports whether some project already owns dir. Exported
// for the CLI, which treats the directory it runs in as the project and so
// always has memory of its own there — but under `octo serve` that same
// directory only gets project memory once it IS a project, so notes written
// from the CLI would go unread there. `octo memory` uses this to say so rather
// than let the difference stay invisible.
func ProjectExistsForDir(dir string) bool {
	if dir == "" {
		return false
	}
	target := memory.NormalizeDir(dir)
	groupMu.Lock()
	defer groupMu.Unlock()
	gf, _, err := cachedRegistry()
	if err != nil {
		return false
	}
	for i := range gf.Groups {
		if wd := gf.Groups[i].WorkingDir; wd != "" && memory.NormalizeDir(wd) == target {
			return true
		}
	}
	return false
}

// newGroupID returns a short random group id ("g-" + 8 hex chars).
func newGroupID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "g-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "g-" + hex.EncodeToString(b[:])
}

// ─── Programmatic group helpers (used by the scheduler) ─────────────────────
//
// Cron tasks file each run's session under a per-task group (see
// internal/server/tasks_handlers.go). These helpers own groupMu themselves, so
// the scheduler path can create/rename/delete groups and add sessions without
// duplicating the load-modify-save cycle the HTTP handlers run inline.

// createSessionGroupNamed creates a new group with the given name and returns
// it. The caller records the group's ID on the task so later runs reuse it.
//
// taskID marks the group as that cron task's run cluster, which is what lets it
// exist without a directory — the one group the user did not create and cannot
// edit. Pass "" from any other caller, in which case workingDir must be set or
// the group would be a plain group and dissolved at the next start.
//
// workingDir, when set, makes the group a project — which is what scopes its
// sessions' memory (see Server.sessionMemDir). A cron task with a directory is
// working on that directory every run, so its notes belong to it rather than to
// the tier every session on the machine reads. An unusable directory is dropped
// rather than failing the call: for a cron task the cluster still works without
// one, and a task must run regardless. Callers that need the directory to have
// landed check WorkingDir on the returned group.
func createSessionGroupNamed(name, workingDir, taskID string) (sessionGroup, error) {
	if workingDir != "" {
		dir, verr := validateWorkingDir(workingDir)
		if verr != nil {
			slog.Warn("group working dir unusable; dropping it",
				"group", name, "dir", workingDir, "err", verr)
			workingDir = ""
		} else {
			workingDir = dir
		}
	}
	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		return sessionGroup{}, err
	}
	g := sessionGroup{ID: newGroupID(), Name: name, SessionIDs: []string{}, WorkingDir: workingDir, TaskID: taskID}
	groups = append(groups, g)
	if err := saveSessionGroups(groups); err != nil {
		return sessionGroup{}, err
	}
	return g, nil
}

// addSessionToGroup prepends a session ID to a group — a newly created
// session shows at the top of its group, matching the newest-first session
// list — enforcing single membership (the session is first removed from every
// other group). Returns an error if the target group no longer exists.
//
// This is a creation-time path: the session-creation handler and the
// scheduler. A loose session can also be filed into a project after creation
// (handleSetSessionGroup, which shares fileSessionInGroup); moving between
// projects stays impossible (see that handler for why).
func addSessionToGroup(groupID, sessionID string) error {
	groupMu.LockWrite()
	defer groupMu.Unlock()
	gf, err := loadRegistryFile()
	if err != nil {
		return err
	}
	if err := fileSessionInGroup(&gf, groupID, sessionID); err != nil {
		return err
	}
	return saveRegistry(gf)
}

// fileSessionInGroup is addSessionToGroup's in-memory half: it moves sessionID
// into groupID within an already-loaded registry, leaving the write to the
// caller. Split out so a caller that must find-or-create a project AND file a
// session into it can do both under one lock and one save — two separate
// locked calls would let another process slip a second project for the same
// directory in between.
func fileSessionInGroup(gf *groupFile, groupID, sessionID string) error {
	groups := gf.Groups
	found := false
	for i := range groups {
		ids := groups[i].SessionIDs[:0]
		for _, existing := range groups[i].SessionIDs {
			if existing != sessionID {
				ids = append(ids, existing)
			}
		}
		groups[i].SessionIDs = ids
		if groups[i].ID == groupID {
			groups[i].SessionIDs = append([]string{sessionID}, groups[i].SessionIDs...)
			// Filing a session is how it becomes the project's current one:
			// creation, a move into the project, or a branch all land here.
			groups[i].LastSessionID = sessionID
			found = true
		}
	}
	if !found {
		return fmt.Errorf("session group %q not found", groupID)
	}
	// Group membership and collapsing are mutually exclusive (handleSetSessionCollapse
	// refuses to collapse a session in a group). Since this is the only path that
	// files a session anywhere, clearing a stale collapsed entry has to happen here
	// for that guard to hold from both directions.
	gf.Groups = groups
	gf.CollapsedSessionIDs = removeID(gf.CollapsedSessionIDs, sessionID)
	return nil
}

// renameSessionGroup renames a group by ID. A no-op (nil) if the group is gone
// — a task's group may have been deleted manually in the UI.
func renameSessionGroup(groupID, name string) error {
	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		return err
	}
	for i := range groups {
		if groups[i].ID == groupID {
			groups[i].Name = name
			return saveSessionGroups(groups)
		}
	}
	return nil
}

// deleteSessionGroup removes a group by ID, leaving its member sessions intact
// (they fall back to ungrouped). A no-op if the group is already gone.
func deleteSessionGroup(groupID string) error {
	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		return err
	}
	out := groups[:0]
	found := false
	for _, g := range groups {
		if g.ID == groupID {
			found = true
			continue
		}
		out = append(out, g)
	}
	if !found {
		return nil
	}
	return saveSessionGroups(out)
}

// ─── GET /api/session-groups ────────────────────────────────────────────────

func (s *Server) handleListSessionGroups(w http.ResponseWriter, r *http.Request) {
	groupMu.Lock()
	gf, err := loadRegistryFile()
	groupMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if gf.Groups == nil {
		gf.Groups = []sessionGroup{}
	}
	if gf.PinnedSessionIDs == nil {
		gf.PinnedSessionIDs = []string{}
	}
	if gf.CollapsedSessionIDs == nil {
		gf.CollapsedSessionIDs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"groups":                gf.Groups,
		"pinned_session_ids":    gf.PinnedSessionIDs,
		"collapsed_session_ids": gf.CollapsedSessionIDs,
	})
}

// ─── POST /api/session-groups ───────────────────────────────────────────────

type createSessionGroupRequest struct {
	Name string `json:"name"`
	// SourceDirs are the external folders the project references, 0..N — a
	// project needs none (writing projects, cron projects). The project's own
	// directory is always generated; it is never one of these.
	SourceDirs []string `json:"source_dirs,omitempty"`
	// WorkingDir is the legacy field: it used to BE the project directory.
	// A client still sending it gets that directory mounted as one more
	// source folder, which preserves the intent ("this project works on that
	// directory") under the workspace model.
	WorkingDir string `json:"working_dir,omitempty"`
}

func (s *Server) handleCreateSessionGroup(w http.ResponseWriter, r *http.Request) {
	var req createSessionGroupRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	raw := req.SourceDirs
	if strings.TrimSpace(req.WorkingDir) != "" {
		raw = append(raw, req.WorkingDir)
	}
	sourceDirs, verr := validateSourceDirs(s.curWorkspaceDir(), raw)
	if verr != nil {
		writeError(w, http.StatusBadRequest, verr.Error())
		return
	}
	if dirNameFor(name) == "" {
		// A name with no character a path can carry is the client's problem,
		// not a server failure.
		writeError(w, http.StatusBadRequest, "name contains no characters usable in a directory name")
		return
	}

	groupMu.LockWrite()
	defer groupMu.Unlock()
	// Workspace generation sits inside the write lock: its exists-check plus
	// mkdir is what keeps two same-named projects off one directory, and only
	// the cross-process lock makes that check-then-create atomic.
	workspace, err := workspaceDirForProject(s.curWorkspaceDir(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := loadSessionGroups()
	if err != nil {
		dropWorkspaceDir(workspace)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	g := sessionGroup{ID: newGroupID(), Name: name, SessionIDs: []string{}, WorkingDir: workspace, SourceDirs: sourceDirs}
	groups = append(groups, g)
	if err := saveSessionGroups(groups); err != nil {
		dropWorkspaceDir(workspace)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The person just mounted these folders — that gesture is the hooks trust
	// grant, recorded at the current fingerprints.
	trustMountedHooks(sourceDirs)
	writeJSON(w, http.StatusOK, map[string]any{"group": g})
}

// ─── PATCH /api/session-groups/{id} ─────────────────────────────────────────

// updateSessionGroupRequest carries the editable group fields. All are
// optional pointers so a request can rename, toggle collapsed, or retarget the
// project directory, without one clobbering the others.
type updateSessionGroupRequest struct {
	Name      *string `json:"name,omitempty"`
	Collapsed *bool   `json:"collapsed,omitempty"`
	// WorkingDir is rejected outright: the workspace is generated at creation
	// and fixed for the project's life. It stays in the request type so the
	// handler can answer an old client with a real explanation instead of
	// silently ignoring the field.
	WorkingDir *string `json:"working_dir,omitempty"`
	// SourceDirs replaces the mount set wholesale (same validation as
	// creation). The change reaches running sessions on their next turn
	// through the freeze identity's source-dirs hash.
	SourceDirs *[]string `json:"source_dirs,omitempty"`
	// LastSessionID records which member session the user is looking at, so
	// entering the project again reopens it. The client follows every switch
	// with this field; the value must name a current member.
	LastSessionID *string `json:"last_session_id,omitempty"`
	// SuperMode sets the project's Super-mode switch. A pointer so "field
	// omitted" (keep the current value) and "explicit false" (turn it off)
	// stay distinguishable — the same shape ReasoningEffort would need.
	SuperMode *bool `json:"super_mode,omitempty"`
}

func (s *Server) handleUpdateSessionGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing group id")
		return
	}
	var req updateSessionGroupRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}
	if req.Name == nil && req.Collapsed == nil && req.WorkingDir == nil && req.SourceDirs == nil && req.LastSessionID == nil && req.SuperMode == nil {
		writeError(w, http.StatusBadRequest, "name, collapsed, source_dirs, last_session_id or super_mode is required")
		return
	}
	var name string
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
	}
	if req.WorkingDir != nil {
		writeError(w, http.StatusBadRequest, "working_dir is fixed at creation: the project's workspace cannot be retargeted or cleared")
		return
	}
	// Validate the mounts before touching the registry, so a bad path leaves
	// the group untouched.
	var newSourceDirs []string
	if req.SourceDirs != nil {
		dirs, verr := validateSourceDirs(s.curWorkspaceDir(), *req.SourceDirs)
		if verr != nil {
			writeError(w, http.StatusBadRequest, verr.Error())
			return
		}
		newSourceDirs = dirs
	}

	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := -1
	for i := range groups {
		if groups[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if req.Name != nil {
		groups[idx].Name = name
	}
	if req.Collapsed != nil {
		groups[idx].Collapsed = *req.Collapsed
	}
	if req.SourceDirs != nil {
		groups[idx].SourceDirs = newSourceDirs
	}
	if req.LastSessionID != nil {
		sid := strings.TrimSpace(*req.LastSessionID)
		member := false
		for _, existing := range groups[idx].SessionIDs {
			if existing == sid {
				member = true
				break
			}
		}
		if !member {
			writeError(w, http.StatusBadRequest, "last_session_id must name a session in this group")
			return
		}
		groups[idx].LastSessionID = sid
	}
	if req.SuperMode != nil {
		v := *req.SuperMode
		groups[idx].SuperMode = &v
	}
	if err := saveSessionGroups(groups); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.SourceDirs != nil {
		// The edit gesture is the hooks trust grant, same as creation.
		trustMountedHooks(newSourceDirs)
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": groups[idx]})
}

// dirInSet reports whether dir names one of dirs, however spelled.
func dirInSet(dir string, dirs []string) bool {
	norm := memory.NormalizeDir(dir)
	for _, d := range dirs {
		if memory.NormalizeDir(d) == norm {
			return true
		}
	}
	return false
}

// ─── DELETE /api/session-groups/{id} ────────────────────────────────────────

// handleDeleteSessionGroup removes a project. With ?sessions=delete its member
// sessions are deleted along with it; without it they are left on disk and become
// tasks.
//
// Deleting the sessions is what the UI asks for, and it is one request rather
// than "delete these ids, then delete the project" so a failure halfway cannot
// leave a project standing over sessions that are already gone. The sessions go
// first: a project whose sessions were deleted but which survived a crash is a
// visible empty row the user can delete again, while the reverse is a set of
// sessions with no row to reach them from.
func (s *Server) handleDeleteSessionGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing group id")
		return
	}
	withSessions := r.URL.Query().Get("sessions") == "delete"
	if withSessions {
		groupMu.Lock()
		groups, lerr := loadSessionGroups()
		var members []string
		if lerr == nil {
			for i := range groups {
				if groups[i].ID == id {
					members = append(members, groups[i].SessionIDs...)
					break
				}
			}
		}
		groupMu.Unlock()
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, lerr.Error())
			return
		}
		if _, failed := s.deleteSessionsByID(members); len(failed) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "some sessions could not be deleted; the project was left in place",
				"failed": failed,
			})
			return
		}
	}

	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := groups[:0]
	found := false
	for _, g := range groups {
		if g.ID == id {
			found = true
			continue
		}
		out = append(out, g)
	}
	if !found {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if err := saveSessionGroups(out); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── PUT /api/session-groups/order ──────────────────────────────────────────

type reorderSessionGroupsRequest struct {
	// IDs is the full group list in the desired order. The frontend's up/down
	// controls submit the whole reordered sequence rather than a single move,
	// so the same endpoint also serves a future drag-to-reorder.
	IDs []string `json:"ids"`
}

// handleReorderSessionGroups rewrites the group order to match the given ID
// sequence. Unknown IDs are ignored; any existing group missing from the
// request is appended in its original relative order, so a stale client view
// can never drop a group.
func (s *Server) handleReorderSessionGroups(w http.ResponseWriter, r *http.Request) {
	var req reorderSessionGroupsRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}

	groupMu.LockWrite()
	defer groupMu.Unlock()
	groups, err := loadSessionGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	byID := make(map[string]sessionGroup, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}
	ordered := make([]sessionGroup, 0, len(groups))
	placed := make(map[string]bool, len(groups))
	for _, id := range req.IDs {
		if g, ok := byID[id]; ok && !placed[id] {
			ordered = append(ordered, g)
			placed[id] = true
		}
	}
	// Preserve any group the request omitted (append in original order).
	for _, g := range groups {
		if !placed[g.ID] {
			ordered = append(ordered, g)
		}
	}

	if err := saveSessionGroups(ordered); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": ordered})
}

// ─── PUT /api/sessions/{id}/group ───────────────────────────────────────────

type setSessionGroupRequest struct {
	GroupID string `json:"group_id"`
}

// handleSetSessionGroup files a loose session into a project — the one
// direction of "move" that works, and the only one offered. This endpoint
// used to refuse every move: back then the facts a project decides (the
// tools' directory, the memory tier, the hooks/sandbox mounts) were treated
// as fixed at creation, and the frozen system prompt had no way to notice a
// membership change. Neither is true anymore: every one of those facts is
// re-derived from projectForSession on each turn, and the freeze identity's
// source-dirs hash (empty for a loose session, never empty for a project —
// see sourceDirsHash) is guaranteed to change when a loose session gains a
// project, so the next turn recomposes the prompt with the project's
// directory, memory tier, and mounts. The transcript's earlier turns keep
// reading as run where they ran — the same accepted inconsistency as a
// session whose working directory is retargeted mid-life, which has always
// been allowed.
//
// Moving OUT of a project, or between projects, stays refused. Those
// directions have no guaranteed re-freeze (two projects with identical
// mounts hash alike), and a session that has absorbed one project's memory
// tier and directory into its transcript doesn't become another project's
// history by re-filing it. One direction covers the actual need — "this
// loose chat turned out to belong to that project" — without reopening the
// rest.
func (s *Server) handleSetSessionGroup(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	var req setSessionGroupRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}
	gid := strings.TrimSpace(req.GroupID)
	if gid == "" {
		writeError(w, http.StatusBadRequest, "group_id is required")
		return
	}
	groupMu.LockWrite()
	defer groupMu.Unlock()
	// Unlike pin/collapse (display state, harmless on a stale ID), filing a
	// nonexistent session would show a ghost row under the project. A stat,
	// not a load — proving the file exists doesn't require parsing a
	// possibly-huge transcript — and inside the lock, so a concurrent delete
	// can't slip between the check and the filing.
	if _, err := agent.SessionMTime(sid); err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	gf, err := loadRegistryFile()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var target *sessionGroup
	for i := range gf.Groups {
		g := &gf.Groups[i]
		if g.ID == gid {
			target = g
		}
		// Membership in a non-project group doesn't block the move: only
		// legacy/hand-edited registries still carry one (dissolvePlainGroups
		// retires them at startup), and fileSessionInGroup pulls the session
		// out of it as part of filing — which is the right outcome.
		if !g.isProject() {
			continue
		}
		for _, id := range g.SessionIDs {
			if id == sid {
				writeError(w, http.StatusConflict,
					"the session is already in a project — that is decided once; start a new session in the target project instead")
				return
			}
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if !target.isProject() {
		writeError(w, http.StatusConflict, "the target group is not a project")
		return
	}
	if err := fileSessionInGroup(&gf, gid, sid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := saveRegistry(gf); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": *target})
}

// ─── PUT /api/sessions/{id}/pin ─────────────────────────────────────────────

type setSessionPinRequest struct {
	Pinned bool `json:"pinned"`
}

// handleSetSessionPin pins or unpins a session. Pinning appends the ID to the
// end of the list (most-recently pinned last); unpinning removes it. The
// operation is idempotent — pinning an already-pinned session keeps its
// position, unpinning an absent one is a no-op.
func (s *Server) handleSetSessionPin(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	var req setSessionPinRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}

	groupMu.LockWrite()
	defer groupMu.Unlock()
	gf, err := loadRegistryFile()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Drop any existing entry first, then re-add at the end when pinning. This
	// keeps the list free of duplicates and gives a stable "unpin" path.
	out := make([]string, 0, len(gf.PinnedSessionIDs)+1)
	for _, id := range gf.PinnedSessionIDs {
		if id != sid {
			out = append(out, id)
		}
	}
	if req.Pinned {
		out = append(out, sid)
		// Pinning and collapsing are mutually exclusive; pinning wins over a
		// stale collapsed entry (e.g. set from another tab) instead of leaving
		// the session in both lists.
		gf.CollapsedSessionIDs = removeID(gf.CollapsedSessionIDs, sid)
	}
	gf.PinnedSessionIDs = out

	if err := saveRegistry(gf); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pinned": req.Pinned})
}

// removeID returns ids without sid, preserving order.
func removeID(ids []string, sid string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id != sid {
			out = append(out, id)
		}
	}
	return out
}

// ─── PUT /api/sessions/{id}/collapse ────────────────────────────────────────

type setSessionCollapsedRequest struct {
	Collapsed bool `json:"collapsed"`
}

// handleSetSessionCollapsed archives a session (hides it from the sidebar's
// Tasks/Projects sections, moves it into Settings' 数据管理), or restores it.
// Archiving keeps whatever project membership the session already has —
// unlike pin, which still can't coexist with it — so restoring puts a project's
// session back exactly where it was, with no second write needed. Idempotent
// like pin: collapsing an already-collapsed session keeps its position,
// restoring an absent one is a no-op.
func (s *Server) handleSetSessionCollapsed(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "missing session id")
		return
	}
	var req setSessionCollapsedRequest
	if err := readBodyJSON(r, &req); err != nil {
		writeInvalidJSONBody(w, err)
		return
	}

	groupMu.LockWrite()
	defer groupMu.Unlock()
	gf, err := loadRegistryFile()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.Collapsed {
		for _, id := range gf.PinnedSessionIDs {
			if id == sid {
				writeError(w, http.StatusConflict, "session is pinned; unpin it before collapsing")
				return
			}
		}
	}

	out := removeID(gf.CollapsedSessionIDs, sid)
	if req.Collapsed {
		out = append(out, sid)
	}
	gf.CollapsedSessionIDs = out

	if err := saveRegistry(gf); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "collapsed": req.Collapsed})
}
