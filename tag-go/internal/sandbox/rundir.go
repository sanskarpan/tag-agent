package sandbox

import (
	"fmt"
	"runtime"
	"strings"
)

// Run-directory validation, shared by every platform that confines a run to a
// directory.
//
// On BOTH macOS and Linux the run directory is the sandbox's read/write
// boundary: the SBPL profile re-allows read+write under it, and the Landlock
// allow-list makes it the ONLY writable tree. That means a run dir which sits AT
// or ABOVE a sensitive path hands back exactly what the policy exists to deny --
// `--dir /` grants the whole filesystem, `--dir $HOME` grants the user's whole
// home (and `--dir` defaults to the current working directory, so `cd ~ && tag
// sandbox run` was enough). Requiring a specific working directory is the
// control working, not a limitation.
//
// The logic below is platform-neutral; only the DATA (which paths count as
// boundaries and which trees are secret) varies per platform, so the same
// reasoning cannot drift between the two backends again.

// sensitiveHomeDirs are credential locations denied for BOTH read and write on
// macOS. They are also run-dir boundaries and secret trees.
var sensitiveHomeDirs = []string{
	".ssh", ".aws", ".gnupg", ".config", ".gcloud", ".kube", ".docker",
	"Library/Keychains",
}

// linuxSensitiveHomeDirs is the Linux equivalent (no macOS Keychains).
var linuxSensitiveHomeDirs = []string{
	".ssh", ".aws", ".gnupg", ".config", ".gcloud", ".kube", ".docker",
}

// dataFirmlinkPrefix is the APFS firmlink root: /System/Volumes/Data/Users/x and
// /Users/x are the same directory. EvalSymlinks does NOT collapse it (a firmlink
// is not a symlink), so run-dir validation normalises it away -- otherwise
// `--dir /System/Volumes/Data/Users/<me>` would spell $HOME past a string check.
// This is a darwin-only concern; normalizeRunDirFor applies it only there.
const dataFirmlinkPrefix = "/System/Volumes/Data"

// runDirPolicy is the per-platform data half of validateRunDir.
type runDirPolicy struct {
	// secretTrees are trees whose CONTENTS are secret: a run dir at or UNDER one
	// is disqualifying (the policy would deny the run its own cwd anyway).
	secretTrees []string
	// boundaries must never be re-opened wholesale: a run dir at or ABOVE one is
	// disqualifying, because re-allowing the run dir would re-open the boundary.
	boundaries []string
}

// darwinRunDirSecretTrees / darwinRunDirBoundaries are the macOS tables. They
// are exactly the set the SBPL profile denies, plus the ancestors that would
// cancel those denials.
var (
	darwinRunDirSecretTrees = []string{"/etc", "/private/etc", "/var/db", "/private/var/db"}
	darwinRunDirBoundaries  = []string{
		"/", "/Users", "/home", "/Volumes", "/System", dataFirmlinkPrefix,
		"/Library", "/private", "/var", "/private/var", "/usr", "/etc", "/private/etc",
		"/var/db", "/private/var/db",
	}
)

// linuxRunDirSecretTrees / linuxRunDirBoundaries are the Linux tables.
//
// Landlock makes the run dir the only writable tree, so every top-level system
// root is a boundary: `--dir /` would grant write over the entire filesystem and
// `--dir /home` over every user's home. /proc, /sys and /dev are listed because
// a run dir there is both nonsensical and (for /proc, /sys) a route to kernel
// state; /nix is listed for NixOS, where /nix/store is the whole system.
var (
	linuxRunDirSecretTrees = []string{"/etc", "/var/db", "/proc", "/sys", "/dev", "/boot"}
	linuxRunDirBoundaries  = []string{
		"/", "/home", "/root", "/etc", "/usr", "/var", "/proc", "/sys", "/dev",
		"/boot", "/lib", "/lib64", "/bin", "/sbin", "/opt", "/srv", "/run", "/nix",
	}
)

// runDirPolicyFor returns the boundary tables for a GOOS, with the caller's home
// directory (and its credential subdirectories) folded in.
//
// An unrecognised GOOS gets the Linux table: those platforms fail closed in
// buildIsolation anyway, and a POSIX-shaped conservative default is the safe
// answer if that ever changes.
func runDirPolicyFor(goos, home string) runDirPolicy {
	var p runDirPolicy
	var homeDirs []string
	switch goos {
	case "darwin":
		p.secretTrees = append(p.secretTrees, darwinRunDirSecretTrees...)
		p.boundaries = append(p.boundaries, darwinRunDirBoundaries...)
		homeDirs = sensitiveHomeDirs
	default:
		p.secretTrees = append(p.secretTrees, linuxRunDirSecretTrees...)
		p.boundaries = append(p.boundaries, linuxRunDirBoundaries...)
		homeDirs = linuxSensitiveHomeDirs
	}
	if home != "" {
		h := normalizeRunDirFor(goos, home)
		// $HOME itself is a boundary: re-allowing it would expose the whole home.
		p.boundaries = append(p.boundaries, h)
		for _, d := range homeDirs {
			s := h + "/" + d
			p.boundaries = append(p.boundaries, s)
			p.secretTrees = append(p.secretTrees, s)
		}
	}
	return p
}

// runDirTooBroadMsg is the fail-closed message for a run dir that is itself a
// sensitive boundary. It mirrors the sandbox-exec-missing path (exit 127) and
// names the escape hatch. The wording is shared so macOS and Linux report the
// same thing.
func runDirTooBroadMsg(runDir, why string) string {
	return fmt.Sprintf("sandbox: refusing to run with %s as the run directory: %s. "+
		"The run directory is the sandbox's read/write boundary, so it must be a specific "+
		"working directory (e.g. a scratch dir under your home), never a sensitive tree or "+
		"one of its ancestors. Pass --dir <subdirectory>, or use --backend docker for "+
		"isolated execution.", runDir, why)
}

// runDirTooBroadIsolation is the Result.Isolation string for that refusal. No
// layer was applied, and the string says so rather than describing the policy we
// would have built.
const runDirTooBroadIsolation = "none (failed closed: run directory too broad to confine)"

// normalizeRunDir strips any platform-specific aliasing and the trailing slash
// so path comparisons see the canonical spelling.
func normalizeRunDir(p string) string { return normalizeRunDirFor(runtime.GOOS, p) }

// normalizeRunDirFor is normalizeRunDir for an explicit GOOS, so tests can
// exercise another platform's rules from this host.
func normalizeRunDirFor(goos, p string) string {
	// APFS firmlinks are a darwin concern; elsewhere the prefix is an ordinary
	// (and almost certainly nonexistent) path and must NOT be rewritten.
	if goos == "darwin" && p != dataFirmlinkPrefix && strings.HasPrefix(p, dataFirmlinkPrefix+"/") {
		p = strings.TrimPrefix(p, dataFirmlinkPrefix)
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		p = "/"
	}
	return p
}

// isAtOrUnder reports whether p is dir itself or lives inside it.
func isAtOrUnder(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// isAtOrAbove reports whether p is dir itself or an ancestor of it: exactly the
// relationship that lets a re-allow of p cancel a denial of dir.
func isAtOrAbove(p, dir string) bool {
	return isAtOrUnder(dir, p)
}

// validateRunDir rejects run directories that are at or above a sensitive
// boundary, or inside a secret tree, for the platform we are running on.
func validateRunDir(runDir, home string) error {
	return validateRunDirFor(runtime.GOOS, runDir, home)
}

// validateRunDirFor is validateRunDir against an explicit GOOS's tables. It
// exists so the Linux tables can be tested from a macOS host (and vice versa),
// where the Linux runtime paths themselves cannot be exercised.
func validateRunDirFor(goos, runDir, home string) error {
	p := normalizeRunDirFor(goos, runDir)
	pol := runDirPolicyFor(goos, home)
	for _, s := range pol.secretTrees {
		if isAtOrUnder(p, s) {
			return fmt.Errorf("%s", runDirTooBroadMsg(runDir, "it is inside the protected tree "+s))
		}
	}
	for _, b := range pol.boundaries {
		if isAtOrAbove(p, b) {
			why := "it is the protected path " + b
			if p != b {
				why = "it contains the protected path " + b
			}
			return fmt.Errorf("%s", runDirTooBroadMsg(runDir, why))
		}
	}
	return nil
}
