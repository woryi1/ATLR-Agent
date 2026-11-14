// Package datahome resolves the profile-scoped root for Octo's persistent user
// data.
//
// Portable layout: the anchor is the directory holding the running executable,
// not the user's home. The default data root is <exe dir>\Data; a profile named
// "work" uses <exe dir>\Data-work. Program, data and workspaces therefore all
// travel together on one drive with no writes outside it.
package datahome

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ProfileEnv carries the selected profile into re-executed serve workers and
// child processes. The selected profile can come from the global --profile
// command-line flag or an inherited environment.
const ProfileEnv = "OCTO_PROFILE"

// PortableRootEnv overrides the portable anchor (the exe directory) for tests
// and setups that need the data tree somewhere specific. When unset, the
// executable's directory is used.
const PortableRootEnv = "OCTO_PORTABLE_ROOT"

// dataDirName is the profile data directory under the portable root. Named
// profiles append "-<name>" (Data-work), mirroring the historical ~/.octo-<name>.
const dataDirName = "Data"

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Root returns the directory anchoring the portable layout: the directory that
// contains the running executable (overridable via PortableRootEnv). Every
// profile data root and the default workspace live below it.
func Root() (string, error) {
	if root := strings.TrimSpace(os.Getenv(PortableRootEnv)); root != "" {
		return filepath.Clean(root), nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	dir := filepath.Dir(exe)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		dir = resolved
	}
	if dir == "" {
		return "", fmt.Errorf("resolve executable directory: empty path")
	}
	return dir, nil
}

// Configure validates profile and records it for the current process. An empty
// profile selects the default Data data root.
func Configure(profile string) error {
	profile = strings.TrimSpace(profile)
	if profile != "" && !profileName.MatchString(profile) {
		return fmt.Errorf("profile must contain only letters, digits, '-' or '_', and start with a letter or digit")
	}
	return os.Setenv(ProfileEnv, profile)
}

// ConfigureFromArgs extracts the global --profile value from any command-line
// position. The returned argument slice contains no profile flags, so command-
// specific flag parsers do not need to know about this global option.
func ConfigureFromArgs(args []string) ([]string, error) {
	filtered := make([]string, 0, len(args))
	profile := ""
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--profile":
			if seen {
				return nil, fmt.Errorf("--profile may be specified only once")
			}
			if i+1 == len(args) || strings.TrimSpace(args[i+1]) == "" {
				return nil, fmt.Errorf("--profile requires a name")
			}
			seen = true
			profile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--profile="):
			if seen {
				return nil, fmt.Errorf("--profile may be specified only once")
			}
			profile = strings.TrimPrefix(arg, "--profile=")
			if strings.TrimSpace(profile) == "" {
				return nil, fmt.Errorf("--profile requires a name")
			}
			seen = true
		default:
			filtered = append(filtered, arg)
		}
	}
	if seen {
		if err := Configure(profile); err != nil {
			return nil, err
		}
	}
	return filtered, nil
}

// Dir returns the profile-scoped root for Octo's persistent user data. The
// default profile uses <exe dir>\Data; a profile named "work" uses
// <exe dir>\Data-work.
//
// The first call also settles the on-disk layout (flat → User/Records/Tools/
// Work/Cache, see layout.go). A failed migration changes nothing and leaves the
// flat paths in use, so a failure is never worse than not upgrading.
func Dir() (string, error) {
	profile := strings.TrimSpace(os.Getenv(ProfileEnv))
	if profile != "" && !profileName.MatchString(profile) {
		return "", fmt.Errorf("invalid %s value %q", ProfileEnv, profile)
	}
	dir, err := DirFor(profile)
	if err != nil {
		return "", err
	}
	_ = ensureLayout(dir)
	return dir, nil
}

// Current returns the selected profile name: "" for the default Data root.
func Current() string {
	return strings.TrimSpace(os.Getenv(ProfileEnv))
}

// ValidName reports whether profile is an acceptable profile name. The empty
// string is valid and names the default root.
func ValidName(profile string) bool {
	return profile == "" || profileName.MatchString(profile)
}

// DirFor returns the data root a given profile would use, whether or not it
// exists on disk, without consulting the selected profile. Management commands
// use it to look at roots other than the one they run under.
func DirFor(profile string) (string, error) {
	if !ValidName(profile) {
		return "", fmt.Errorf("invalid profile name %q", profile)
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	name := dataDirName
	if profile != "" {
		name += "-" + profile
	}
	return filepath.Join(root, name), nil
}

// Path joins elements below the profile-scoped data root. The first element is
// a logical name (sessions, config.yml, webview …); once the regioned layout is
// in place it lands under its region folder (Records/sessions, User/config.yml,
// Cache/webview …). Call sites keep using the logical name, which is what makes
// the split reversible: without the layout marker the old flat path is returned.
func Path(elem ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if len(elem) > 0 && layoutVersion(dir) >= LayoutVersion {
		if region, ok := regionOf(elem[0]); ok {
			return filepath.Join(append([]string{dir, region}, elem...)...), nil
		}
	}
	return filepath.Join(append([]string{dir}, elem...)...), nil
}

// SharedPath joins elements below <exe dir>\Data regardless of the selected
// profile — the home for things that belong to the machine rather than to one
// profile's user data. Helper binaries the installers stage live here, and so
// does the desktop shell's record of which profile to open, which by definition
// cannot live inside a profile.
func SharedPath(elem ...string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	base := filepath.Join(root, dataDirName)
	// Shared paths belong to the default tree, so migrating it here is what
	// keeps Data\Tools\bin and Data\Cache\webview resolvable from any profile.
	_ = ensureLayout(base)
	return filepath.Join(append([]string{base}, elem...)...), nil
}

// BinDir returns the machine-scoped directory for Octo-managed helper binaries.
func BinDir() (string, error) { return SharedPath(RegionTools, "bin") }

// List returns the profiles that exist on disk: "" for the default Data root
// when it is there, then each named Data-<name>, sorted. It reports what has
// been used, not what is allowed — any valid name works whether or not it is
// listed.
func List() ([]string, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read portable root %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch name := e.Name(); {
		case name == dataDirName:
			out = append(out, "")
		case strings.HasPrefix(name, dataDirName+"-"):
			profile := strings.TrimPrefix(name, dataDirName+"-")
			if profileName.MatchString(profile) {
				out = append(out, profile)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
