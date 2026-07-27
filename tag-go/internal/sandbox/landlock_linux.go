package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock (Linux >= 5.13) gives us CGO-free, unprivileged filesystem
// confinement. It is an allow-list LSM: everything the ruleset "handles" is
// denied unless a path_beneath rule re-grants it.
//
// Landlock must be installed in the *child* between fork and exec, and Go has
// no preexec_fn equivalent. We therefore use the well-known re-exec trick
// (as docker/libcontainer does): the parent launches its own binary with a
// marker argv[1] plus an env-encoded policy; the init() below intercepts that
// very early, installs Landlock, and then execve()s the real command. The
// marker is only ever set by us, and both the argv marker and the env variable
// must be present for the helper path to trigger.

const (
	helperArg = "__tag_sandbox_landlock_helper__"
	helperEnv = "TAG_SANDBOX_LANDLOCK_POLICY"
	// helperExitPolicy is used when the helper cannot install the policy it was
	// asked to install. Failing closed beats running unconfined.
	helperExitPolicy = 126
)

// Landlock filesystem access-right groups, by ABI level.
const (
	llFSBaseV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	llFSRefer    = unix.LANDLOCK_ACCESS_FS_REFER     // ABI 2
	llFSTruncate = unix.LANDLOCK_ACCESS_FS_TRUNCATE  // ABI 3
	llFSIoctlDev = unix.LANDLOCK_ACCESS_FS_IOCTL_DEV // ABI 5

	llFSRead    = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	llFSExecute = unix.LANDLOCK_ACCESS_FS_EXECUTE
	// llFSReadWriteFile is read+write on a single file (no directory rights).
	llFSReadWriteFile = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE
)

// llRulesetAttr mirrors struct landlock_ruleset_attr. Only the first `size`
// bytes we pass to the kernel are read, so older ABIs get an 8-byte view.
type llRulesetAttr struct {
	accessFS  uint64
	accessNet uint64
	scoped    uint64
}

// llPathBeneath mirrors struct landlock_path_beneath_attr, which the kernel
// defines as packed (12 bytes: u64 + s32). landlock_add_rule takes no size
// argument -- the kernel copies exactly its own 12 bytes from our pointer -- so
// the trailing padding Go adds is never read. The explicit pad field documents
// that and keeps the struct 8-byte aligned.
type llPathBeneath struct {
	allowedAccess uint64
	parentFD      int32
	_             int32
}

// landlockABI returns the kernel's supported Landlock ABI version, or 0 when
// Landlock is unavailable (kernel < 5.13, or the LSM is not enabled).
func landlockABI() int {
	r, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(r)
}

// llHandledFS is the set of access rights the ruleset governs for a given ABI.
func llHandledFS(abi int) uint64 {
	h := uint64(llFSBaseV1)
	if abi >= 2 {
		h |= llFSRefer
	}
	if abi >= 3 {
		h |= llFSTruncate
	}
	if abi >= 5 {
		h |= llFSIoctlDev
	}
	return h
}

// llRule is one allow-list entry.
type llRule struct {
	path   string
	access uint64
}

// llPolicy is the serialisable policy handed to the re-exec helper.
type llPolicy struct {
	abi   int
	rules []llRule
	// blockNet requests Landlock ABI>=4 TCP scoping (connect + bind).
	blockNet bool
}

// encode renders the policy into a single env-var value:
//
//	abi;net;<access>:<path>|<access>:<path>|...
//
// Paths may contain any byte except '|' and newline; we reject those rather
// than invent an escaping scheme (the run dir is the only caller-controlled
// path and such names are vanishingly rare).
func (p llPolicy) encode() (string, error) {
	var parts []string
	for _, r := range p.rules {
		if strings.ContainsAny(r.path, "|\n") {
			return "", fmt.Errorf("sandbox: refusing to build a Landlock policy for path %q (contains '|' or newline)", r.path)
		}
		parts = append(parts, fmt.Sprintf("%d:%s", r.access, r.path))
	}
	net := 0
	if p.blockNet {
		net = 1
	}
	return fmt.Sprintf("%d;%d;%s", p.abi, net, strings.Join(parts, "|")), nil
}

func decodePolicy(s string) (llPolicy, error) {
	var p llPolicy
	head := strings.SplitN(s, ";", 3)
	if len(head) != 3 {
		return p, fmt.Errorf("malformed landlock policy")
	}
	if _, err := fmt.Sscanf(head[0], "%d", &p.abi); err != nil {
		return p, fmt.Errorf("malformed landlock policy abi: %w", err)
	}
	p.blockNet = head[1] == "1"
	if head[2] == "" {
		return p, nil
	}
	for _, item := range strings.Split(head[2], "|") {
		i := strings.Index(item, ":")
		if i < 0 {
			return p, fmt.Errorf("malformed landlock rule %q", item)
		}
		var acc uint64
		if _, err := fmt.Sscanf(item[:i], "%d", &acc); err != nil {
			return p, fmt.Errorf("malformed landlock rule %q: %w", item, err)
		}
		p.rules = append(p.rules, llRule{path: item[i+1:], access: acc})
	}
	return p, nil
}

// apply installs the policy on the calling thread/process. It is irreversible.
func (p llPolicy) apply() error {
	handled := llHandledFS(p.abi)
	attr := llRulesetAttr{accessFS: handled}
	size := uintptr(8)
	if p.abi >= 4 {
		size = 16
		if p.blockNet {
			attr.accessNet = unix.LANDLOCK_ACCESS_NET_CONNECT_TCP | unix.LANDLOCK_ACCESS_NET_BIND_TCP
		}
	}
	if p.abi >= 6 {
		size = 24
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer unix.Close(rulesetFD)

	for _, r := range p.rules {
		pfd, err := unix.Open(r.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			// A path that does not exist on this system simply contributes no
			// rule; that is a tightening, never a loosening.
			continue
		}
		pb := llPathBeneath{allowedAccess: r.access & handled, parentFD: int32(pfd)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(rulesetFD),
			unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&pb)), 0, 0, 0)
		unix.Close(pfd)
		if errno != 0 {
			return fmt.Errorf("landlock_add_rule(%s): %w", r.path, errno)
		}
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// init intercepts the re-exec helper invocation. It runs before any CLI wiring
// and either execs the real command or exits; it never returns to normal
// program flow. Both the argv marker and the env policy must be present.
func init() {
	if len(os.Args) < 3 || os.Args[1] != helperArg {
		return
	}
	// argv[1] is our private marker, so reaching here means WE spawned this
	// process to install a policy. A missing/empty policy env var therefore means
	// the policy was lost in transit -- falling through to normal CLI flow would
	// run the command with NO Landlock at all, so fail closed instead.
	spec := os.Getenv(helperEnv)
	if spec == "" {
		fmt.Fprintln(os.Stderr, "sandbox: Landlock helper invoked without a policy; refusing to run unconfined")
		os.Exit(helperExitPolicy)
	}
	policy, err := decodePolicy(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(helperExitPolicy)
	}
	if err := policy.apply(); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: failed to install Landlock policy:", err)
		os.Exit(helperExitPolicy)
	}
	_ = os.Unsetenv(helperEnv)
	argv := os.Args[2:]
	target := argv[0]
	if !strings.Contains(target, "/") {
		resolved, lerr := exec.LookPath(target)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "sandbox: %s: %v\n", target, lerr)
			os.Exit(127)
		}
		target = resolved
	}
	if err := unix.Exec(target, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: exec %s: %v\n", target, err)
		os.Exit(helperExitPolicy)
	}
}
