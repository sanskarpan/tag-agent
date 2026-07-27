package sandbox

import (
	"strings"
	"testing"
)

// Run-dir validation is shared by macOS and Linux, so its table tests run on
// ANY host: validateRunDirFor takes the GOOS explicitly. That matters because
// the Linux boundary list cannot otherwise be exercised from a macOS developer
// machine, which is exactly how the Linux backend ended up without the check.

// runDirCases is one platform's expected verdicts.
type runDirCases struct {
	home   string
	reject []string
	accept []string
}

// darwinRunDirCases: every macOS boundary and secret tree, plus the ordinary
// scratch dirs that must keep working.
func darwinRunDirCases() runDirCases {
	const home = "/Users/nobody"
	return runDirCases{
		home: home,
		reject: []string{
			"/", "/Users", "/Users/", "/home", "/Volumes", "/System", "/System/Volumes/Data",
			"/private", "/var", "/private/var", "/etc", "/private/etc", "/var/db", "/usr",
			"/Library",
			home, home + "/", // $HOME itself
			home + "/.ssh", home + "/.ssh/keys", home + "/.aws", home + "/Library/Keychains",
			home + "/Library",                        // ancestor of ~/Library/Keychains
			"/System/Volumes/Data/Users/nobody",      // firmlink spelling of $HOME
			"/System/Volumes/Data/Users/nobody/.ssh", // firmlink spelling of a secret tree
		},
		accept: []string{
			home + "/scratch", home + "/scratch/deep", home + "/Documents/proj",
			"/private/tmp/x", "/private/var/folders/ab/cd/T/go-build123", "/Volumes/Ext/proj",
			"/usr/local/build", "/opt/work",
			"/System/Volumes/Data/Users/nobody/scratch", // firmlink spelling of a scratch dir
		},
	}
}

// linuxRunDirCases: the Linux boundary list. Landlock makes the run dir the
// ONLY writable tree, so `--dir /` used to grant write over the whole
// filesystem and `--dir $HOME` over the user's entire home.
func linuxRunDirCases() runDirCases {
	const home = "/home/nobody"
	return runDirCases{
		home: home,
		reject: []string{
			"/", "/home", "/home/", "/root", "/etc", "/usr", "/var", "/proc", "/sys",
			"/dev", "/boot", "/lib", "/lib64", "/bin", "/sbin", "/opt", "/srv", "/run",
			"/nix",
			"/etc/ssl/certs", "/proc/self", "/dev/shm", "/var/db", "/boot/efi", // inside a secret tree
			home, home + "/", // $HOME itself
			home + "/.ssh", home + "/.ssh/keys", home + "/.aws", home + "/.gnupg",
			home + "/.config", home + "/.config/tag", home + "/.gcloud", home + "/.kube",
			home + "/.docker",
		},
		accept: []string{
			"/tmp/x", "/tmp/build/deep", "/var/tmp/build",
			home + "/scratch", home + "/scratch/deep", home + "/projects/tag",
			"/usr/local/build", "/opt/work", "/srv/www", "/run/user/1000/scratch",
			"/nix/store/abc-src",
			// Another user's tree is not OUR boundary (POSIX permissions are the
			// guard there); macOS treats /Users/other the same way.
			"/home/other/proj",
			// The APFS firmlink prefix is meaningless on Linux and must NOT be
			// rewritten into "$HOME" by normalizeRunDir.
			"/System/Volumes/Data/Users/nobody",
		},
	}
}

// assertRunDirTable runs one platform's table through validateRunDirFor.
func assertRunDirTable(t *testing.T, goos string, c runDirCases) {
	t.Helper()
	for _, d := range c.reject {
		err := validateRunDirFor(goos, d, c.home)
		if err == nil {
			t.Errorf("[%s] validateRunDir(%q) accepted a run dir at/above a protected boundary", goos, d)
			continue
		}
		// The refusal must be actionable: it always names the escape hatch.
		if !strings.Contains(err.Error(), "--backend docker") {
			t.Errorf("[%s] validateRunDir(%q) error should name the escape hatch, got %q", goos, d, err)
		}
	}
	for _, d := range c.accept {
		if err := validateRunDirFor(goos, d, c.home); err != nil {
			t.Errorf("[%s] validateRunDir(%q) rejected an ordinary run dir: %v", goos, d, err)
		}
	}
}

// TestValidateRunDirDarwinTable pins the macOS verdicts from any host.
func TestValidateRunDirDarwinTable(t *testing.T) {
	assertRunDirTable(t, "darwin", darwinRunDirCases())
}

// TestValidateRunDirLinuxTable is the cross-platform half of the fix: the same
// boundary check the macOS backend had is now applied to the Landlock backend.
// It is compile- and logic-checked here even though the Linux runtime paths
// cannot be executed on a macOS host.
func TestValidateRunDirLinuxTable(t *testing.T) {
	assertRunDirTable(t, "linux", linuxRunDirCases())
}

// TestValidateRunDirNoHome: with no resolvable home the platform boundaries
// still apply (and the check must not crash or accept "/").
func TestValidateRunDirNoHome(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		if err := validateRunDirFor(goos, "/", ""); err == nil {
			t.Errorf("[%s] / accepted as a run dir with no home resolved", goos)
		}
		if err := validateRunDirFor(goos, "/tmp/scratch", ""); err != nil {
			// /tmp is not a boundary on either platform.
			t.Errorf("[%s] /tmp/scratch rejected with no home resolved: %v", goos, err)
		}
	}
}

// TestNormalizeRunDirIsPlatformScoped: the APFS firmlink collapse is a darwin
// concern only. Applying it on Linux would rewrite a real (if unusual) path.
func TestNormalizeRunDirIsPlatformScoped(t *testing.T) {
	cases := []struct{ goos, in, want string }{
		{"darwin", "/System/Volumes/Data/Users/x", "/Users/x"},
		{"darwin", "/System/Volumes/Data", "/System/Volumes/Data"},
		{"darwin", "/Users/x/", "/Users/x"},
		{"darwin", "/", "/"},
		{"darwin", "//", "/"},
		{"linux", "/System/Volumes/Data/Users/x", "/System/Volumes/Data/Users/x"},
		{"linux", "/home/x/", "/home/x"},
		{"linux", "/", "/"},
	}
	for _, c := range cases {
		if got := normalizeRunDirFor(c.goos, c.in); got != c.want {
			t.Errorf("normalizeRunDirFor(%q, %q) = %q, want %q", c.goos, c.in, got, c.want)
		}
	}
}

// TestRunDirPathRelations pins the two primitives the whole check rests on.
func TestRunDirPathRelations(t *testing.T) {
	if !isAtOrUnder("/anything", "/") {
		t.Error(`isAtOrUnder(x, "/") must always be true: / contains everything`)
	}
	if isAtOrUnder("/usrlocal", "/usr") {
		t.Error("isAtOrUnder must compare path components, not string prefixes")
	}
	if !isAtOrUnder("/usr/local", "/usr") || !isAtOrUnder("/usr", "/usr") {
		t.Error("isAtOrUnder missed a genuine containment")
	}
	if !isAtOrAbove("/", "/etc") || !isAtOrAbove("/etc", "/etc") {
		t.Error("isAtOrAbove missed a genuine ancestor")
	}
	if isAtOrAbove("/etc", "/") {
		t.Error("/etc is not an ancestor of /")
	}
}

// TestRunDirPolicyCoversRequiredBoundaries states the per-platform boundary
// lists as a contract, so shrinking one is a deliberate, visible act.
func TestRunDirPolicyCoversRequiredBoundaries(t *testing.T) {
	required := map[string][]string{
		"darwin": {"/", "/Users", "/home", "/Volumes", "/System", "/System/Volumes/Data",
			"/Library", "/private", "/var", "/private/var", "/usr", "/etc"},
		"linux": {"/", "/home", "/root", "/etc", "/usr", "/var", "/proc", "/sys", "/dev",
			"/boot", "/lib", "/lib64", "/bin", "/sbin", "/opt", "/srv", "/run", "/nix"},
	}
	for goos, want := range required {
		home := "/home/nobody"
		if goos == "darwin" {
			home = "/Users/nobody"
		}
		pol := runDirPolicyFor(goos, home)
		have := make(map[string]bool, len(pol.boundaries))
		for _, b := range pol.boundaries {
			have[b] = true
		}
		for _, w := range append(want, home) {
			if !have[w] {
				t.Errorf("[%s] boundary list is missing %q", goos, w)
			}
		}
		// Credential dirs are boundaries AND secret trees on both platforms.
		secret := make(map[string]bool, len(pol.secretTrees))
		for _, s := range pol.secretTrees {
			secret[s] = true
		}
		for _, d := range []string{".ssh", ".aws", ".gnupg", ".config", ".gcloud", ".kube", ".docker"} {
			p := home + "/" + d
			if !have[p] || !secret[p] {
				t.Errorf("[%s] credential dir %q must be both a boundary and a secret tree", goos, p)
			}
		}
	}
}
