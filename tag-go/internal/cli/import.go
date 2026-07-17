package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tag-agent/tag/internal/importer"
	"github.com/tag-agent/tag/internal/paths"
)

// impSpec describes one import-* command: the source-tool name shown to users,
// the flag used to override the source location, and the run function.
type impSpec struct {
	name     string // e.g. "import-codex"
	display  string // e.g. "Codex"
	short    string
	srcFlag  string // e.g. "codex-home" ("" if none)
	srcUsage string
	oauth    bool // whether to expose --use-oauth
	noAuth   string
	run      func(profileDir, profile, src string, useOAuth bool) (importer.Result, error)
}

// impResolveProfile validates the profile against the config and returns its
// on-disk home dir (created if missing), matching the Python contract.
func impResolveProfile(app *App, profile string) (string, error) {
	profiles := app.Cfg.Profiles()
	if _, ok := profiles[profile]; !ok {
		avail := make([]string, 0, len(profiles))
		for p := range profiles {
			avail = append(avail, p)
		}
		sort.Strings(avail)
		return "", fmt.Errorf("unknown profile %q; available: %s", profile, strings.Join(avail, ", "))
	}
	dir := paths.ProfileHome(app.Cfg.String("runtime.home_dir", ""), profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// impRun executes one importer spec, handling the shared validation, no-auth
// and output plumbing.
func impRun(app *App, s impSpec, profile, src string, useOAuth bool) error {
	dir, err := impResolveProfile(app, profile)
	if err != nil {
		return err
	}
	res, err := s.run(dir, profile, src, useOAuth)
	if err != nil {
		return err
	}
	if res.Status == importer.StatusSkipped {
		if flagJSON {
			_ = emitJSON(res)
		}
		return fmt.Errorf("%s", s.noAuth)
	}
	if flagJSON {
		return emitJSON(res)
	}
	if len(res.Providers) > 0 {
		fmt.Printf("Imported %s credentials into profile '%s' (%s).\n", s.display, profile, strings.Join(res.Providers, ", "))
	} else if res.Mode != "" {
		fmt.Printf("Imported %s credentials into profile '%s' (mode: %s).\n", s.display, profile, res.Mode)
	} else {
		fmt.Printf("Imported %s credentials into profile '%s'.\n", s.display, profile)
	}
	if res.TOSWarn != "" {
		fmt.Printf("WARNING: %s\n", res.TOSWarn)
	}
	return nil
}

// registerImports adds every import-* command to root.
func registerImports(root *cobra.Command, app *App) {
	specs := []impSpec{
		{
			name: "import-codex", display: "Codex",
			short:   "Import existing Codex CLI credentials into a TAG-managed profile",
			srcFlag: "codex-home", srcUsage: "path to the source CODEX_HOME (default: ~/.codex)",
			noAuth: "No importable Codex CLI tokens found. Run `codex login` or set OPENAI_API_KEY in ~/.codex/auth.json.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportCodex(dir, p, src)
			},
		},
		{
			name: "import-claude", display: "Claude",
			short:   "Import Claude Code / Anthropic API credentials into a TAG-managed profile",
			srcFlag: "claude-home", srcUsage: "path to source ~/.claude directory (default: ~/.claude)",
			oauth:  true,
			noAuth: "No Claude credentials found. Set ANTHROPIC_API_KEY or use `tag import-claude --use-oauth` to import from claude auth login.",
			run: func(dir, p, src string, oauth bool) (importer.Result, error) {
				return importer.ImportClaude(dir, p, src, oauth)
			},
		},
		{
			name: "import-gemini", display: "Gemini",
			short:   "Import Gemini CLI / Google API credentials into a TAG-managed profile",
			srcFlag: "gemini-home", srcUsage: "path to source ~/.gemini directory (default: ~/.gemini)",
			oauth:  true,
			noAuth: "No Gemini credentials found. Set GEMINI_API_KEY (from https://aistudio.google.com/app/apikey) or use `tag import-gemini --use-oauth` to import from ~/.gemini/oauth_creds.json.",
			run: func(dir, p, src string, oauth bool) (importer.Result, error) {
				return importer.ImportGemini(dir, p, src, oauth)
			},
		},
		{
			name: "import-continue", display: "Continue.dev",
			short:   "Import API keys from a Continue.dev config into a TAG-managed profile",
			srcFlag: "continue-home", srcUsage: "path to source ~/.continue directory (default: ~/.continue)",
			noAuth: "No Continue.dev config found with API keys. Expected ~/.continue/config.yaml or ~/.continue/config.json.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportContinue(dir, p, src)
			},
		},
		{
			name: "import-mistral", display: "Mistral",
			short:   "Import a Mistral API key from the Mistral Vibe CLI into a TAG-managed profile",
			srcFlag: "vibe-home", srcUsage: "path to source ~/.vibe directory (default: ~/.vibe)",
			noAuth: "No Mistral credentials found. Set MISTRAL_API_KEY or ensure `mistral-vibe` has written ~/.vibe/.env.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportMistral(dir, p, src)
			},
		},
		{
			name: "import-opencode", display: "opencode",
			short:   "Import API keys from opencode auth.json into a TAG-managed profile",
			srcFlag: "opencode-data-dir", srcUsage: "path to opencode data dir (default: ~/.local/share/opencode)",
			noAuth: "No opencode credentials found. Expected ~/.local/share/opencode/auth.json.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportOpencode(dir, p, src)
			},
		},
		{
			name: "import-zed", display: "Zed",
			short:   "Import API keys from Zed editor settings.json into a TAG-managed profile",
			srcFlag: "zed-config", srcUsage: "path to Zed settings.json (default: ~/.config/zed/settings.json)",
			noAuth: "No API keys found in Zed settings. Zed stores keys in the OS keychain by default; set keys via Zed's Agent Settings panel and ensure they are also exported as standard env vars (OPENAI_API_KEY, ANTHROPIC_API_KEY, etc.).",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportZed(dir, p, src)
			},
		},
		{
			name: "import-copilot", display: "GitHub Copilot",
			short:   "Import a GitHub OAuth token from the gh CLI into a TAG-managed profile",
			srcFlag: "gh-config", srcUsage: "path to gh CLI hosts.yml (default: ~/.config/gh/hosts.yml)",
			noAuth: "No GitHub token found. Run `gh auth login` to authenticate the gh CLI, or set GITHUB_TOKEN in your environment.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportCopilot(dir, p, src)
			},
		},
		{
			name: "import-aider", display: "Aider",
			short:   "Import API keys from Aider config into a TAG-managed profile",
			srcFlag: "aider-home", srcUsage: "base directory for Aider config files (default: ~)",
			noAuth: "No Aider credentials found. Expected ~/.aider.conf.yml, ~/.env, or ~/.aider.env with at least one of: OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, etc.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportAider(dir, p, src)
			},
		},
		{
			name: "import-aws", display: "AWS",
			short:   "Import AWS (Bedrock) credentials into a TAG-managed profile",
			srcFlag: "aws-dir", srcUsage: "path to source ~/.aws directory (default: ~/.aws)",
			noAuth: "No AWS credentials found. Set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or run `aws configure` to populate ~/.aws/credentials.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportAWS(dir, p, src)
			},
		},
		{
			name: "import-cursor", display: "Cursor",
			short:   "Import BYOK API keys from the Cursor editor into a TAG-managed profile",
			srcFlag: "cursor-dir", srcUsage: "path to the dir containing Cursor's state.vscdb (default: platform globalStorage)",
			noAuth: "No Cursor API keys found. Add your own API keys in Cursor Settings > Models (BYOK); Cursor stores them in its SQLite state.vscdb.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportCursor(dir, p, src)
			},
		},
		{
			name: "import-supermemory", display: "Supermemory",
			short:   "Import a Supermemory API key into a TAG-managed profile",
			srcFlag: "supermemory-dir", srcUsage: "path to a dir containing Supermemory config.json (default: ~/.config/supermemory)",
			noAuth: "No Supermemory API key found. Set SUPERMEMORY_API_KEY or add an api_key to ~/.config/supermemory/config.json. Get a key at https://supermemory.ai/.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportSupermemory(dir, p, src)
			},
		},
		{
			name: "import-honcho", display: "Honcho",
			short:   "Import Honcho credentials into a TAG-managed profile",
			srcFlag: "honcho-dir", srcUsage: "path to a dir containing Honcho .env or config.yaml (default: ~/.honcho, ~/.config/honcho)",
			noAuth: "No Honcho credentials found. Set HONCHO_API_KEY or add an api_key to ~/.config/honcho/config.yaml. See https://honcho.dev/.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportHoncho(dir, p, src)
			},
		},
		{
			name: "import-nous-portal", display: "Nous Portal",
			short:   "Import a Nous Portal API key and enable the tool gateway for a profile",
			srcFlag: "nous-dir", srcUsage: "path to a dir containing Nous Portal portal.json/config.json (default: ~/.config/nousresearch, ~/.nousresearch)",
			noAuth: "No Nous Portal credentials found. Set NOUS_PORTAL_API_KEY or add an api_key to ~/.config/nousresearch/portal.json. Requires an active subscription (https://portal.nousresearch.com/).",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportNousPortal(dir, p, src)
			},
		},
		{
			name: "import-docker", display: "Docker",
			short:   "Configure the Docker execution backend for a TAG-managed profile",
			srcFlag: "docker-dir", srcUsage: "path to source ~/.docker directory (default: ~/.docker)",
			noAuth: "No Docker configuration found. Run `docker login` to populate ~/.docker/config.json, or set DOCKER_DEFAULT_IMAGE in your environment.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportDocker(dir, p, src)
			},
		},
		{
			name: "import-ssh", display: "SSH",
			short:   "Configure the SSH execution backend for a TAG-managed profile",
			srcFlag: "ssh-dir", srcUsage: "path to source ~/.ssh directory (default: ~/.ssh)",
			noAuth: "No SSH connection details found. Set SSH_HOST (and optionally SSH_USER/SSH_KEY_FILE/SSH_PORT), or add a Host entry to ~/.ssh/config.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportSSH(dir, p, src)
			},
		},
		{
			name: "import-modal", display: "Modal",
			short:   "Configure the Modal execution backend for a TAG-managed profile",
			srcFlag: "modal-config", srcUsage: "path to Modal token file (default: ~/.modal.toml)",
			noAuth: "No Modal credentials found. Set MODAL_TOKEN_ID and MODAL_TOKEN_SECRET, or run `modal token new` to populate ~/.modal.toml.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportModal(dir, p, src)
			},
		},
		{
			name: "import-daytona", display: "Daytona",
			short:   "Configure the Daytona execution backend for a TAG-managed profile",
			srcFlag: "daytona-dir", srcUsage: "path to a dir containing Daytona config.json (default: ~/.config/daytona)",
			noAuth: "No Daytona credentials found. Set DAYTONA_API_KEY (and optionally DAYTONA_WORKSPACE_ID), or add an api_key to ~/.config/daytona/config.json.",
			run: func(dir, p, src string, _ bool) (importer.Result, error) {
				return importer.ImportDaytona(dir, p, src)
			},
		},
	}

	for _, s := range specs {
		s := s
		var profile, src string
		var useOAuth bool
		cmd := &cobra.Command{
			Use:     s.name,
			Short:   s.short,
			GroupID: "system",
			RunE: func(cmd *cobra.Command, args []string) error {
				return impRun(app, s, profile, src, useOAuth)
			},
		}
		cmd.Flags().StringVar(&profile, "profile", "", "target profile (required)")
		_ = cmd.MarkFlagRequired("profile")
		if s.srcFlag != "" {
			cmd.Flags().StringVar(&src, s.srcFlag, "", s.srcUsage)
		}
		if s.oauth {
			cmd.Flags().BoolVar(&useOAuth, "use-oauth", false, "import an OAuth session token (provider ToS may prohibit third-party use)")
		}
		root.AddCommand(cmd)
	}
}
