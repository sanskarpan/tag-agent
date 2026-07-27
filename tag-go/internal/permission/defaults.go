package permission

// Built-in tool names the defaults reason about.
const (
	ToolBash      = "bash"
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
	ToolListDir   = "list_dir"
	ToolWebSearch = "web_search"
)

const (
	// SourceBuiltinCredential marks the credential-path denies, which sit ABOVE a
	// user's catch-all `default:` (see Resolve) so a broad "default: allow" cannot
	// silently unprotect secrets. An explicit per-tool or patterned rule still wins.
	SourceBuiltinCredential = "builtin:credentials"
	// SourceBuiltin marks the ordinary per-tool defaults.
	SourceBuiltin = "builtin"
)

// CredentialRules are the always-on protections for credential-shaped paths.
// They are ordered: the `.env.example` carve-out comes FIRST so a template file
// stays readable while `.env` / `.env.local` / `.env.production` do not.
//
// They apply to every KindPath tool (read_file, write_file, list_dir, and any
// future path tool), never to KindCommand — a shell command is not a path.
func CredentialRules() []Rule {
	deny := func(pat string) Rule {
		return Rule{Tool: "*", Kind: KindPath, Pattern: pat, Action: Deny, Source: SourceBuiltinCredential}
	}
	return []Rule{
		// Carve-out first: committed example templates carry no secrets.
		{Tool: "*", Kind: KindPath, Pattern: "*.env.example", Action: Allow, Source: SourceBuiltinCredential},
		{Tool: "*", Kind: KindPath, Pattern: "*.env.sample", Action: Allow, Source: SourceBuiltinCredential},
		{Tool: "*", Kind: KindPath, Pattern: "*.env.template", Action: Allow, Source: SourceBuiltinCredential},
		deny("*.env"),   // .env, prod.env
		deny("*.env.*"), // .env.local, .env.production
		deny("~/.ssh"),
		deny("~/.ssh/**"),
		deny("~/.aws"),
		deny("~/.aws/**"),
		deny("~/.gnupg"),
		deny("~/.gnupg/**"),
		deny("~/.config/gcloud/**"),
		deny("id_rsa"),
		deny("id_rsa.pub"),
		deny("id_ed25519"),
		deny("id_ed25519.pub"),
		deny("*.pem"),
		deny("*.key"),
		deny("*.p12"),
		deny("*.pfx"),
		deny(".netrc"),
		deny(".npmrc"),
		deny(".pypirc"),
		deny("credentials.json"),
		deny("service-account*.json"),
	}
}

// BuiltinDefaults are the least-specific rules: the per-tool defaults plus the
// trailing catch-all. Reading and listing the (already root-confined) workspace
// is `allow`; anything that mutates the host or executes code is `ask`, and
// `ask` with no TTY is a deny. Note bash is NOT allow — that is the whole point.
func BuiltinDefaults() []Rule {
	r := func(tool string, a Action) Rule {
		return Rule{Tool: tool, Action: a, Source: SourceBuiltin}
	}
	return []Rule{
		r(ToolReadFile, Allow),
		r(ToolListDir, Allow),
		r(ToolWriteFile, Ask),
		r(ToolBash, Ask),
		// web_search is off unless the operator passes --web AND supplies an Exa
		// key, so registration is already the explicit opt-in; it is read-only.
		r(ToolWebSearch, Allow),
		// Catch-all. Unknown tools (MCP bridges, plugins) require approval.
		{Tool: "*", Action: Ask, Source: SourceBuiltin},
	}
}

// DefaultPolicy is the policy used when nothing is configured: secure defaults,
// no auto-approve. With no Prompter this denies bash and write_file.
func DefaultPolicy() Policy {
	rules := append([]Rule{}, CredentialRules()...)
	rules = append(rules, BuiltinDefaults()...)
	return Policy{Rules: rules}
}

// UnsafeAllowAllGuard returns a Guard that approves every tool call. It exists
// for the `--dangerously-allow-all` escape hatch and for tests that need to
// exercise a tool's own logic (path confinement, timeouts) without the consent
// gate short-circuiting them. The name is deliberately loud and greppable:
// production wiring must never call it.
func UnsafeAllowAllGuard() *Guard {
	return NewGuard(Policy{Rules: []Rule{{Tool: "*", Action: Allow, Source: "unsafe-allow-all"}}}, nil, nil)
}
