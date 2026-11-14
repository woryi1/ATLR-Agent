package datahome

// Layout v2: the data root used to be a flat pile — config, sessions, skills,
// caches and the browser profile all side by side. It is now split into five
// regions so the answer to "what can I delete / what should I back up" is a
// folder name rather than a list of file names:
//
//	Data/
//	  User/     settings, credentials, identity files, themes, uploads
//	  Records/  sessions, memories, tasks, workflow journals, audit log
//	  Tools/    skills, agents, MCP config/tokens, bin
//	  Work/     project + task workspaces (the user's own output)
//	  Cache/    WebView2 profile, music cache, trash, tmp, logs
//
// Everything the application reads goes through datahome.Path, so the split is
// implemented as a name → region mapping rather than 40 call-site edits. The
// mapping only applies once the layout marker exists: if the migration fails,
// nothing is written and the old flat paths keep working, which is what makes
// the upgrade safe to abort.
//
// Paths that must survive the split:
//   - Path() keeps working before AND after migration (same logical name).
//   - The migration is idempotent: the marker is the last thing written.
//   - Workspace folders move too, and the session transcripts that point at
//     them are rewritten, otherwise old sessions open on a directory that no
//     longer exists.

import (
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Region names. They are also the folder names under the data root.
const (
	RegionUser    = "User"
	RegionRecords = "Records"
	RegionTools   = "Tools"
	RegionWork    = "Work"
	RegionCache   = "Cache"
)

// LayoutVersion is the on-disk layout this build expects. The marker file lives
// at <data root>/.layout; absent means "still flat, migrate on first use".
const LayoutVersion = 2

const (
	layoutFile      = ".layout"
	layoutLockFile  = ".layout.lock"
	migrateLogFile  = "migrate.log"
	workspaceFolder = "Workspace"
)

// regionFor maps a legacy top-level entry to its region. Names not listed stay
// at the data root (the layout marker, the desktop profile pointer and the
// migration log, which must be readable before the split).
var regionFor = map[string]string{
	// User: settings, credentials, identity.
	"config.yml": RegionUser, "config.yaml": RegionUser, "config.yml.bak": RegionUser,
	"desktop.json": RegionUser, "bili-cookies.json": RegionUser,
	"soul.md": RegionUser, "user.md": RegionUser, "AGENTS.md": RegionUser, "octorules.md": RegionUser,
	"permissions.yml": RegionUser, "hooks.yml": RegionUser, "hooks-trust.json": RegionUser,
	"hooks-pending": RegionUser, "channels.yml": RegionUser, "im-bindings.json": RegionUser,
	"weixin-credentials.json": RegionUser, "serve.env": RegionUser,
	"history": RegionUser, "input_history": RegionUser,
	"themes": RegionUser, "uploads": RegionUser, "backgrounds": RegionUser,
	"light-apps": RegionUser, "landing": RegionUser,
	".onboard_attempted": RegionUser, ".onboard_skipped": RegionUser,

	// Records: history the user would miss.
	"sessions": RegionRecords, "session-groups.json": RegionRecords, "memories": RegionRecords,
	"tasks": RegionRecords, "audit.log": RegionRecords, "workflow-journals": RegionRecords,
	"databases": RegionRecords, "browser-recordings": RegionRecords,

	// Tools: things the agent loads.
	"mcp.json": RegionTools, "mcp-tokens": RegionTools,
	"skills": RegionTools, "skills-default": RegionTools, "skills-expert": RegionTools,
	"agents": RegionTools, "agents-default": RegionTools,
	"workflows": RegionTools, "workflows-default": RegionTools,
	"site-patterns": RegionTools, "browser-skills": RegionTools, "bin": RegionTools,

	// Cache: safe to delete.
	"webview": RegionCache, "music-cache": RegionCache,
	"music-playlist.json": RegionCache, "music-state.json": RegionCache,
	"trash": RegionCache, "tmp": RegionCache, "logs": RegionCache,
	"crash.log": RegionCache, "serve.log": RegionCache, "cli.log": RegionCache,
	"serve.pid": RegionCache, "serve.addr": RegionCache,
}

// requiredRegions are always present after a successful migration, even when
// the user had no data in them yet.
var requiredRegions = []string{RegionUser, RegionRecords, RegionTools, RegionWork, RegionCache}

// legacyLeftovers are entries no code reads any more: an old backup of the
// skills tree and ad-hoc scripts someone dropped into the data directory. They
// are parked under Cache/removed-legacy instead of the root — deleting Cache
// (the "safe to delete" folder) is what finally removes them, so the migration
// itself never destroys anything outright.
var legacyLeftovers = []string{"skills-backup", "tools"}

// bestEffort entries are pure cache: if something still has a handle on them
// (WebView2 keeps its profile open for a while after a crash, and the desktop
// shell has certainly been force-killed more than once), leaving the old folder
// behind is harmless. The layout still completes, and a later start reclaims it.
var bestEffort = map[string]bool{
	"webview": true, "music-cache": true, "trash": true, "tmp": true, "logs": true,
	"crash.log": true, "serve.log": true, "cli.log": true, "serve.pid": true,
	"serve.addr": true, "music-playlist.json": true, "music-state.json": true,
}

var layoutOnce sync.Map // data dir -> *sync.Once

// regionOf returns the region for a legacy entry name, honouring the ".lock"
// siblings the config/session registry leave behind.
func regionOf(name string) (string, bool) {
	if r, ok := regionFor[name]; ok {
		return r, true
	}
	if base, ok := strings.CutSuffix(name, ".lock"); ok {
		if r, ok := regionFor[base]; ok {
			return r, true
		}
	}
	return "", false
}

// RegionPath joins elements under one region of the data root. It does not
// depend on the layout version: the region folder is the destination whether or
// not the migration has run, so callers that create their own paths can use it
// directly.
func RegionPath(region string, elem ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir, region}, elem...)...), nil
}

// EnsureLayout runs the flat → regioned migration for the current profile.
// Safe to call repeatedly; exported so the desktop shell can migrate before it
// hands a path to WebView2.
func EnsureLayout() error {
	dir, err := DirFor(Current())
	if err != nil {
		return err
	}
	return ensureLayout(dir)
}

func ensureLayout(dir string) error {
	onceAny, _ := layoutOnce.LoadOrStore(dir, &sync.Once{})
	once := onceAny.(*sync.Once)
	var err error
	once.Do(func() {
		err = migrateLayout(dir)
		// Runs even when the layout was already settled: a cache folder that
		// was locked during the migration gets reclaimed on a later start.
		reclaimStaleCache(dir)
	})
	return err
}

// ReclaimStaleCache retries the best-effort cache moves for the current
// profile. Exported for the desktop shell, which calls it a few seconds after
// start: the process's own log files and a just-closed WebView2 are still held
// during the first moments, and the next start is a long wait for a cache file.
func ReclaimStaleCache() {
	dir, err := DirFor(Current())
	if err != nil {
		return
	}
	reclaimStaleCache(dir)
}

// reclaimStaleCache retries the best-effort cache moves that a previous run
// could not do. Silent on failure: a locked folder just waits for the next
// start (or a reboot).
func reclaimStaleCache(dir string) {
	for name := range bestEffort {
		region, ok := regionOf(name)
		if !ok {
			continue
		}
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(dir, region, name)
		if _, err := os.Stat(dst); err == nil {
			if name != "webview" {
				continue
			}
			// The live WebView2 profile now owns Cache\webview; park the old
			// one beside it instead of merging two profile trees.
			dst = filepath.Join(dir, region, name+"-old")
			if _, err := os.Stat(dst); err == nil {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			continue
		}
		migrateNote(dir, "reclaimed cache "+name)
	}
}

func layoutMarker(dir string) string { return filepath.Join(dir, layoutFile) }

func layoutVersion(dir string) int {
	data, err := os.ReadFile(layoutMarker(dir))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

type moveRecord struct {
	from string
	to   string
}

// migrateLayout performs the one-time split. Every step is a same-volume
// rename, so a failure leaves the old tree intact after rollback and the caller
// keeps using flat paths (the marker is never written).
func migrateLayout(dir string) error {
	if layoutVersion(dir) >= LayoutVersion {
		return nil
	}
	root := filepath.Dir(dir)
	// A fresh install has no data root yet; the region folders and the marker
	// are the first things written into it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// A second process may be migrating the same tree. The lock is advisory:
	// a stale one (crashed migration) is taken over after ten minutes.
	lock := filepath.Join(dir, layoutLockFile)
	if f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err != nil {
		if st, statErr := os.Stat(lock); statErr == nil && time.Since(st.ModTime()) > 10*time.Minute {
			_ = os.Remove(lock)
		}
		// Another process is doing the move right now. Wait for its marker
		// instead of running on flat paths while the tree is mid-flight; the
		// move is renames only, so this is normally a few milliseconds.
		for i := 0; i < 50; i++ {
			if layoutVersion(dir) >= LayoutVersion {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	} else {
		_ = f.Close()
	}
	defer os.Remove(lock)

	// Park the leftovers no code reads any more BEFORE the region folders are
	// created: one of them is a lowercase "tools", and Windows would treat
	// "Tools" as the same directory, so the region could never be created while
	// it is still there.
	leftovers := filepath.Join(dir, RegionCache, "removed-legacy")
	var moves []moveRecord
	rollback := func(cause error) error {
		for i := len(moves) - 1; i >= 0; i-- {
			_ = os.Rename(moves[i].to, moves[i].from)
		}
		writeMigrateLog(dir, cause)
		return cause
	}
	for _, name := range legacyLeftovers {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.MkdirAll(leftovers, 0o755); err != nil {
			return rollback(err)
		}
		dst := filepath.Join(leftovers, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return rollback(fmt.Errorf("park %s: %w", name, err))
		}
		moves = append(moves, moveRecord{from: src, to: dst})
	}

	for _, region := range requiredRegions {
		if err := os.MkdirAll(filepath.Join(dir, region), 0o755); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == layoutFile || name == layoutLockFile || name == migrateLogFile {
			continue
		}
		region, ok := regionOf(name)
		if !ok {
			continue
		}
		src := filepath.Join(dir, name)
		dst := filepath.Join(dir, region, name)
		if _, err := os.Stat(dst); err == nil {
			// Destination already exists (a half-finished earlier run or a
			// file recreated by an old build). Leave both; never overwrite.
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			if bestEffort[name] {
				// Locked cache: leave it, finish the split, retry next start.
				writeMigrateLog(dir, fmt.Errorf("cache %s stays for now: %w", name, err))
				continue
			}
			return rollback(fmt.Errorf("move %s: %w", name, err))
		}
		moves = append(moves, moveRecord{from: src, to: dst})
	}

	// Workspaces only move once, from the default profile's tree: the folder
	// lives beside the exe, not inside a profile.
	var workspaceMoves []moveRecord
	if filepath.Clean(dir) == filepath.Clean(filepath.Join(root, dataDirName)) {
		workspaceMoves, err = migrateWorkspace(root, dir)
		if err != nil {
			return rollback(err)
		}
		moves = append(moves, workspaceMoves...)
	}

	if err := rewriteSessionPaths(dir, root, workspaceMoves); err != nil {
		return rollback(err)
	}
	renameMemorySlugs(dir, workspaceMoves)

	if err := os.WriteFile(layoutMarker(dir), []byte(strconv.Itoa(LayoutVersion)+"\n"), 0o644); err != nil {
		return rollback(fmt.Errorf("write layout marker: %w", err))
	}
	writeMigrateLog(dir, nil)
	return nil
}

// migrateWorkspace folds <exe dir>\Workspace into <data root>\Work: task
// sessions go to Work\Tasks, project folders to Work itself. The empty source
// folder is removed so the portable root keeps exactly one data directory.
func migrateWorkspace(root, dir string) ([]moveRecord, error) {
	src := filepath.Join(root, workspaceFolder)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return nil, nil
	}
	work := filepath.Join(dir, RegionWork)
	tasks := filepath.Join(work, "Tasks")
	for _, p := range []string{work, tasks, filepath.Join(work, "Backups")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, err
		}
	}

	var moves []moveRecord
	children, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		from := filepath.Join(src, c.Name())
		dest := filepath.Join(work, c.Name())
		if strings.EqualFold(c.Name(), "tasks") {
			// Workspace\tasks\<session id> → Work\Tasks\<session id>
			ids, err := os.ReadDir(from)
			if err != nil {
				return moves, err
			}
			for _, id := range ids {
				f := filepath.Join(from, id.Name())
				t := filepath.Join(tasks, id.Name())
				if _, err := os.Stat(t); err == nil {
					continue
				}
				if err := os.Rename(f, t); err != nil {
					return moves, fmt.Errorf("move workspace task %s: %w", id.Name(), err)
				}
				moves = append(moves, moveRecord{from: f, to: t})
			}
			continue
		}
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		if err := os.Rename(from, dest); err != nil {
			return moves, fmt.Errorf("move workspace %s: %w", c.Name(), err)
		}
		moves = append(moves, moveRecord{from: from, to: dest})
	}
	// Remove what is left only if it is empty: anything unreadable stays put
	// rather than being deleted.
	_ = os.Remove(filepath.Join(src, "tasks"))
	_ = os.Remove(src)
	return moves, nil
}

// rewriteSessionPaths fixes the absolute paths recorded inside session
// transcripts (working_dir / cwd / composed_for_cwd). They are plain JSON
// lines, so this replaces the escaped path text rather than re-encoding the
// object — re-encoding would reorder fields and rewrite numbers.
func rewriteSessionPaths(dir, root string, workspaceMoves []moveRecord) error {
	sessions, err := os.Stat(filepath.Join(dir, RegionRecords, "sessions"))
	if err != nil || !sessions.IsDir() {
		return nil
	}
	subs := workspaceSubstitutions(dir, root, workspaceMoves)
	if len(subs) == 0 {
		return nil
	}
	sessionsDir := filepath.Join(dir, RegionRecords, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if err := rewriteFile(filepath.Join(sessionsDir, e.Name()), subs); err != nil {
			return err
		}
	}
	// The project registry stores each group's working dir too.
	groups := filepath.Join(dir, RegionRecords, "session-groups.json")
	if _, err := os.Stat(groups); err == nil {
		if err := rewriteFile(groups, subs); err != nil {
			return err
		}
	}
	return nil
}

// workspaceSubstitutions turns the moved folders into raw-text replacements,
// longest source first so Work\Tasks wins over Work.
func workspaceSubstitutions(dir, root string, moves []moveRecord) [][2]string {
	var pairs [][2]string
	add := func(old, new string) {
		if old == "" || new == "" || old == new {
			return
		}
		pairs = append(pairs, [2]string{old, new})
	}
	// Every regioned entry keeps its old absolute prefix in old transcripts
	// (an upload the model referenced, a skills path baked into a frozen system
	// prompt). Rewrite those too, so nothing points at a folder that moved.
	for name, region := range regionFor {
		add(filepath.Join(dir, name), filepath.Join(dir, region, name))
	}
	workTasks := filepath.Join(dir, RegionWork, "Tasks")
	work := filepath.Join(dir, RegionWork)
	// Every workspace move is expressed generically first…
	for _, m := range moves {
		add(m.from, m.to)
	}
	// …then the two historical roots that no longer exist on disk but are still
	// recorded by older sessions.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, "Octo", "tasks"), workTasks)
		add(filepath.Join(home, "Octo"), work)
	}
	add(filepath.Join(root, workspaceFolder, "tasks"), workTasks)
	add(filepath.Join(root, workspaceFolder), work)

	// Longest old prefix first.
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if len(pairs[j][0]) > len(pairs[i][0]) {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}
	return pairs
}

func rewriteFile(path string, subs [][2]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	changed := false
	for _, pair := range subs {
		for _, form := range []string{pair[0], filepath.ToSlash(pair[0])} {
			for _, to := range []string{pair[1], filepath.ToSlash(pair[1])} {
				// JSON escapes backslashes; plain text does not. Replace both
				// spellings so a transcript never keeps a stale path.
				escOld := strings.ReplaceAll(form, `\`, `\\`)
				escNew := strings.ReplaceAll(to, `\`, `\\`)
				if strings.Contains(text, escOld) {
					text = strings.ReplaceAll(text, escOld, escNew)
					changed = true
				}
				if strings.Contains(text, form) {
					text = strings.ReplaceAll(text, form, to)
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	tmp := path + ".layout-tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// renameMemorySlugs keeps project memories attached to their project after the
// workspace moves: the memory folder is named "<basename>-<fnv32(path)>", so a
// new path means a new slug.
func renameMemorySlugs(dir string, moves []moveRecord) {
	if len(moves) == 0 {
		return
	}
	memories := filepath.Join(dir, RegionRecords, "memories")
	for _, m := range moves {
		oldSlug, newSlug := slugFor(m.from), slugFor(m.to)
		if oldSlug == newSlug {
			continue
		}
		from := filepath.Join(memories, oldSlug)
		to := filepath.Join(memories, newSlug)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if _, err := os.Stat(to); err == nil {
			continue
		}
		_ = os.Rename(from, to)
	}
}

// slugFor mirrors memory.dirSlug without importing the memory package (which
// imports this one).
func slugFor(projectDir string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(projectDir))
	base := slugify(filepath.Base(projectDir))
	if base == "" {
		return fmt.Sprintf("project-%08x", h.Sum32())
	}
	return fmt.Sprintf("%s-%08x", base, h.Sum32())
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 50 {
		out = strings.Trim(out[:50], "-")
	}
	return out
}

// writeMigrateLog leaves a short breadcrumb: a successful split, or why it was
// rolled back. Without a console the user would otherwise have no clue.
func writeMigrateLog(dir string, cause error) {
	if cause == nil {
		migrateNote(dir, "layout v"+strconv.Itoa(LayoutVersion)+" ok")
		return
	}
	migrateNote(dir, "layout migration failed, rolled back: "+cause.Error())
}

// migrateNote appends one line to the layout log.
func migrateNote(dir, text string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + text
	f, err := os.OpenFile(filepath.Join(dir, migrateLogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.WriteString(f, line+"\n")
}
