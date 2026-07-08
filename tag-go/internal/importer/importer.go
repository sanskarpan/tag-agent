// Package importer ports the TAG `import-*` credential importers from
// src/tag/controller.py (the import_*_into_profile functions). Each importer
// reads a source agent-tool's local credentials and writes the extracted API
// key(s) into a TAG profile's .env file (owner-only, mode 0600).
//
// Everything here is fully offline: no network or model calls are ever made.
package importer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Result mirrors the dict returned by the Python import_*_into_profile helpers.
type Result struct {
	Profile   string   `json:"profile"`
	Status    string   `json:"status"`
	Mode      string   `json:"mode,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Providers []string `json:"providers_imported,omitempty"`
	Source    string   `json:"source,omitempty"`
	TOSWarn   string   `json:"tos_warning,omitempty"`
}

const (
	StatusImported = "imported"
	StatusSkipped  = "skipped-no-auth"
)

// ---------------------------------------------------------------------------
// .env helpers (ports of core/utils.py)
// ---------------------------------------------------------------------------

func sanitizeEnvValue(v string) string {
	r := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\x00", "")
	return r.Replace(v)
}

// UpsertEnvLine writes or replaces KEY=VALUE in envFile without disturbing other
// lines, then chmods the file to 0600. Port of utils._upsert_env_line.
func UpsertEnvLine(envFile, key, value string) error {
	value = sanitizeEnvValue(value)
	if err := os.MkdirAll(filepath.Dir(envFile), 0o755); err != nil {
		return err
	}
	var lines []string
	if b, err := os.ReadFile(envFile); err == nil {
		text := string(b)
		text = strings.TrimRight(text, "\n")
		if text != "" {
			lines = strings.Split(text, "\n")
		}
	}
	prefix := key + "="
	newLine := key + "=" + value
	replaced := false
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if !replaced && strings.HasPrefix(strings.TrimSpace(line), prefix) {
			out = append(out, newLine)
			replaced = true
		} else {
			out = append(out, line)
		}
	}
	if !replaced {
		out = append(out, newLine)
	}
	body := strings.Join(out, "\n") + "\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(envFile, 0o600)
	return nil
}

// readDotenv parses a shell-style .env file into a map. Port of utils.read_dotenv.
func readDotenv(path string) map[string]string {
	values := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[len("export"):], " \t")
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		value := strings.TrimSpace(parts[1])
		if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		} else if idx := strings.Index(value, " #"); idx != -1 {
			value = strings.TrimRight(value[:idx], " ")
		}
		values[key] = value
	}
	return values
}

// ---------------------------------------------------------------------------
// path helpers
// ---------------------------------------------------------------------------

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func envKey(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func sortedKeysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// writeKeys upserts every key in found and returns a sorted provider list.
func writeKeys(envFile string, found map[string]string) ([]string, error) {
	ks := sortedKeysOf(found)
	for _, k := range ks {
		if err := UpsertEnvLine(envFile, k, found[k]); err != nil {
			return nil, err
		}
	}
	return ks, nil
}

func envFileFor(profileDir string) string { return filepath.Join(profileDir, ".env") }

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

// ImportCodex reads OPENAI_API_KEY from <codexHome>/auth.json. If sourceHome is
// empty the default (env TAG_IMPORT_CODEX_HOME or ~/.codex) is used.
func ImportCodex(profileDir, profile, sourceHome string) (Result, error) {
	if sourceHome == "" {
		sourceHome = envKey("TAG_IMPORT_CODEX_HOME")
	}
	if sourceHome == "" {
		sourceHome = filepath.Join(homeDir(), ".codex")
	}
	authFile := filepath.Join(sourceHome, "auth.json")
	b, err := os.ReadFile(authFile)
	if err != nil {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	var data map[string]any
	if json.Unmarshal(b, &data) != nil {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	key := strings.TrimSpace(asString(data["OPENAI_API_KEY"]))
	if key == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	if err := UpsertEnvLine(envFileFor(profileDir), "OPENAI_API_KEY", key); err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "codex", Source: authFile}, nil
}

// ---------------------------------------------------------------------------
// Claude
// ---------------------------------------------------------------------------

// ImportClaude writes ANTHROPIC_API_KEY (from env) or, with useOAuth, the OAuth
// access token from <claudeHome>/.credentials.json or ~/.claude.json.
func ImportClaude(profileDir, profile, sourceHome string, useOAuth bool) (Result, error) {
	if k := envKey("ANTHROPIC_API_KEY"); k != "" {
		if err := UpsertEnvLine(envFileFor(profileDir), "ANTHROPIC_API_KEY", k); err != nil {
			return Result{}, err
		}
		return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "anthropic"}, nil
	}
	if useOAuth {
		token, source := detectClaudeOAuth(sourceHome)
		if token != "" {
			if err := UpsertEnvLine(envFileFor(profileDir), "CLAUDE_CODE_OAUTH_TOKEN", token); err != nil {
				return Result{}, err
			}
			return Result{
				Profile: profile, Status: StatusImported, Mode: "oauth", Provider: "anthropic", Source: source,
				TOSWarn: "Anthropic prohibits use of claude auth login OAuth tokens in third-party tools. Set ANTHROPIC_API_KEY for ToS-compliant access.",
			}, nil
		}
	}
	return Result{Profile: profile, Status: StatusSkipped}, nil
}

func detectClaudeOAuth(sourceHome string) (token, source string) {
	claudeHome := sourceHome
	if claudeHome == "" {
		claudeHome = filepath.Join(homeDir(), ".claude")
	}
	credsFile := filepath.Join(claudeHome, ".credentials.json")
	if b, err := os.ReadFile(credsFile); err == nil {
		var data map[string]any
		if json.Unmarshal(b, &data) == nil {
			if t := strings.TrimSpace(nestedStr(data, "claudeAiOauth", "accessToken")); t != "" {
				return t, credsFile
			}
		}
	}
	dotClaude := filepath.Join(homeDir(), ".claude.json")
	if b, err := os.ReadFile(dotClaude); err == nil {
		var data map[string]any
		if json.Unmarshal(b, &data) == nil {
			if t := strings.TrimSpace(nestedStr(data, "claudeAiOauth", "accessToken")); t != "" {
				return t, dotClaude
			}
			if t := strings.TrimSpace(nestedStr(data, "oauthAccount", "accessToken")); t != "" {
				return t, dotClaude
			}
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// Gemini
// ---------------------------------------------------------------------------

// ImportGemini writes GEMINI_API_KEY (from env or <geminiHome>/.env) or, with
// useOAuth, writes an auth/google_oauth.json into the profile dir.
func ImportGemini(profileDir, profile, sourceHome string, useOAuth bool) (Result, error) {
	geminiHome := sourceHome
	if geminiHome == "" {
		geminiHome = filepath.Join(homeDir(), ".gemini")
	}
	apiKey := envKey("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = strings.TrimSpace(readDotenv(filepath.Join(geminiHome, ".env"))["GEMINI_API_KEY"])
	}
	if apiKey != "" {
		if err := UpsertEnvLine(envFileFor(profileDir), "GEMINI_API_KEY", apiKey); err != nil {
			return Result{}, err
		}
		return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "gemini"}, nil
	}
	if useOAuth {
		oauthFile := filepath.Join(geminiHome, "oauth_creds.json")
		if b, err := os.ReadFile(oauthFile); err == nil {
			var data map[string]any
			if json.Unmarshal(b, &data) == nil {
				access := strings.TrimSpace(asString(data["access_token"]))
				refresh := strings.TrimSpace(asString(data["refresh_token"]))
				if access != "" || refresh != "" {
					out := map[string]any{
						"access_token":  access,
						"refresh_token": refresh,
						"expiry_date":   data["expiry_date"],
						"source":        "gemini-cli-import",
					}
					dir := filepath.Join(profileDir, "auth")
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return Result{}, err
					}
					jb, _ := json.MarshalIndent(out, "", "  ")
					if err := os.WriteFile(filepath.Join(dir, "google_oauth.json"), jb, 0o600); err != nil {
						return Result{}, err
					}
					return Result{
						Profile: profile, Status: StatusImported, Mode: "oauth", Provider: "google-gemini-cli", Source: oauthFile,
						TOSWarn: "Google explicitly prohibits piggybacking on Gemini CLI OAuth tokens in third-party tools. Use GEMINI_API_KEY from https://aistudio.google.com/app/apikey for ToS-compliant access.",
					}, nil
				}
			}
		}
	}
	return Result{Profile: profile, Status: StatusSkipped}, nil
}

// ---------------------------------------------------------------------------
// Continue.dev
// ---------------------------------------------------------------------------

var continueProviderEnvMap = map[string]string{
	"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "google": "GEMINI_API_KEY",
	"gemini": "GEMINI_API_KEY", "mistral": "MISTRAL_API_KEY", "deepseek": "DEEPSEEK_API_KEY",
	"xai": "XAI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "huggingface": "HF_TOKEN",
	"nvidia": "NVIDIA_API_KEY", "groq": "GROQ_API_KEY", "together": "TOGETHER_API_KEY",
	"cohere": "COHERE_API_KEY", "fireworks": "FIREWORKS_API_KEY", "perplexity": "PERPLEXITY_API_KEY",
}

// ImportContinue reads models[].{provider,apiKey} from <home>/config.yaml and
// config.json, resolving localEnv: references from the environment.
func ImportContinue(profileDir, profile, sourceHome string) (Result, error) {
	home := sourceHome
	if home == "" {
		home = filepath.Join(homeDir(), ".continue")
	}
	found := map[string]string{}
	resolveKey := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "localEnv:") {
			return envKey(raw[len("localEnv:"):])
		}
		return raw
	}
	extract := func(models []any) {
		for _, m := range models {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(asString(mm["provider"])))
			apiKey := resolveKey(firstStr(mm, "apiKey", "api_key"))
			env := continueProviderEnvMap[provider]
			if env != "" && apiKey != "" {
				if _, dup := found[env]; !dup {
					found[env] = apiKey
				}
			}
		}
	}
	if data, ok := loadYAMLMap(filepath.Join(home, "config.yaml")); ok {
		extract(asSliceAny(data["models"]))
	}
	if data, ok := loadJSONMap(filepath.Join(home, "config.json")); ok {
		extract(asSliceAny(data["models"]))
	}
	return finishMulti(profileDir, profile, found)
}

// ---------------------------------------------------------------------------
// Mistral
// ---------------------------------------------------------------------------

// ImportMistral writes MISTRAL_API_KEY from env or <vibeHome>/.env.
func ImportMistral(profileDir, profile, sourceHome string) (Result, error) {
	key := envKey("MISTRAL_API_KEY")
	source := ""
	if key == "" {
		vibeHome := sourceHome
		if vibeHome == "" {
			vibeHome = filepath.Join(homeDir(), ".vibe")
		}
		dotenv := filepath.Join(vibeHome, ".env")
		key = strings.TrimSpace(readDotenv(dotenv)["MISTRAL_API_KEY"])
		if key != "" {
			source = dotenv
		}
	}
	if key == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	if err := UpsertEnvLine(envFileFor(profileDir), "MISTRAL_API_KEY", key); err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "mistral", Source: source}, nil
}

// ---------------------------------------------------------------------------
// opencode
// ---------------------------------------------------------------------------

var opencodeProviderEnvMap = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY", "google": "GEMINI_API_KEY",
	"google-vertex-ai": "GEMINI_API_KEY", "groq": "GROQ_API_KEY", "deepseek": "DEEPSEEK_API_KEY",
	"openrouter": "OPENROUTER_API_KEY", "xai": "XAI_API_KEY", "mistral": "MISTRAL_API_KEY",
	"fireworks": "FIREWORKS_API_KEY", "together": "TOGETHER_API_KEY", "perplexity": "PERPLEXITY_API_KEY",
	"cohere": "COHERE_API_KEY", "nvidia": "NVIDIA_API_KEY", "github": "GITHUB_TOKEN",
}

// ImportOpencode reads <dataDir>/auth.json (default ~/.local/share/opencode).
func ImportOpencode(profileDir, profile, sourceDataDir string) (Result, error) {
	dataDir := sourceDataDir
	if dataDir == "" {
		dataDir = filepath.Join(homeDir(), ".local", "share", "opencode")
	}
	found := map[string]string{}
	if data, ok := loadJSONMap(filepath.Join(dataDir, "auth.json")); ok {
		for provider, cred := range data {
			cm, ok := cred.(map[string]any)
			if !ok || asString(cm["type"]) != "api" {
				continue
			}
			key := strings.TrimSpace(asString(cm["key"]))
			env := opencodeProviderEnvMap[strings.ToLower(provider)]
			if key != "" && env != "" {
				if _, dup := found[env]; !dup {
					found[env] = key
				}
			}
		}
	}
	return finishMulti(profileDir, profile, found)
}

// ---------------------------------------------------------------------------
// Zed
// ---------------------------------------------------------------------------

var zedProviderEnvMap = map[string]string{
	"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "google": "GEMINI_API_KEY",
	"mistral": "MISTRAL_API_KEY", "deepseek": "DEEPSEEK_API_KEY", "xai": "XAI_API_KEY",
	"groq": "GROQ_API_KEY",
}

// ImportZed reads language_models{provider:{api_key}} from Zed's settings.json
// (JSONC). Default ~/.config/zed/settings.json.
func ImportZed(profileDir, profile, sourceConfig string) (Result, error) {
	settings := sourceConfig
	if settings == "" {
		settings = filepath.Join(homeDir(), ".config", "zed", "settings.json")
	}
	found := map[string]string{}
	if b, err := os.ReadFile(settings); err == nil {
		if data, ok := loadJSONC(b); ok {
			if lm, ok := data["language_models"].(map[string]any); ok {
				for provider, block := range lm {
					bm, ok := block.(map[string]any)
					if !ok {
						continue
					}
					key := strings.TrimSpace(asString(bm["api_key"]))
					env := zedProviderEnvMap[strings.ToLower(provider)]
					if key != "" && env != "" {
						if _, dup := found[env]; !dup {
							found[env] = key
						}
					}
				}
			}
		}
	}
	return finishMulti(profileDir, profile, found)
}

// ---------------------------------------------------------------------------
// GitHub Copilot
// ---------------------------------------------------------------------------

// ImportCopilot writes GITHUB_TOKEN from env (GITHUB_TOKEN/GH_TOKEN) or from the
// gh CLI hosts.yml (github.com.oauth_token/token). Default ~/.config/gh/hosts.yml.
func ImportCopilot(profileDir, profile, sourceGhConfig string) (Result, error) {
	token := envKey("GITHUB_TOKEN")
	if token == "" {
		token = envKey("GH_TOKEN")
	}
	source := ""
	if token == "" {
		hostsFile := sourceGhConfig
		if hostsFile == "" {
			hostsFile = filepath.Join(homeDir(), ".config", "gh", "hosts.yml")
		}
		if data, ok := loadYAMLMap(hostsFile); ok {
			if gh, ok := data["github.com"].(map[string]any); ok {
				t := strings.TrimSpace(asString(gh["oauth_token"]))
				if t == "" {
					t = strings.TrimSpace(asString(gh["token"]))
				}
				if t != "" {
					token = t
					source = hostsFile
				}
			}
		}
	}
	if token == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	if err := UpsertEnvLine(envFileFor(profileDir), "GITHUB_TOKEN", token); err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "oauth_token", Provider: "github-copilot", Source: source}, nil
}

// ---------------------------------------------------------------------------
// Aider
// ---------------------------------------------------------------------------

var aiderYAMLKeyMap = map[string]string{
	"openai-api-key": "OPENAI_API_KEY", "anthropic-api-key": "ANTHROPIC_API_KEY",
	"gemini-api-key": "GEMINI_API_KEY", "deepseek-api-key": "DEEPSEEK_API_KEY",
	"openrouter-api-key": "OPENROUTER_API_KEY", "mistral-api-key": "MISTRAL_API_KEY",
	"groq-api-key": "GROQ_API_KEY", "xai-api-key": "XAI_API_KEY",
	"cohere-api-key": "COHERE_API_KEY", "perplexity-api-key": "PERPLEXITY_API_KEY",
}

var aiderAPIKeyPrefixMap = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY", "openrouter": "OPENROUTER_API_KEY",
	"mistral": "MISTRAL_API_KEY", "groq": "GROQ_API_KEY", "deepseek": "DEEPSEEK_API_KEY",
	"xai": "XAI_API_KEY", "cohere": "COHERE_API_KEY", "perplexity": "PERPLEXITY_API_KEY",
	"together": "TOGETHER_API_KEY", "fireworks": "FIREWORKS_API_KEY",
}

var aiderDotenvKeys = []string{
	"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY",
	"MISTRAL_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY", "XAI_API_KEY",
	"PERPLEXITY_API_KEY", "COHERE_API_KEY", "TOGETHER_API_KEY", "FIREWORKS_API_KEY",
}

// ImportAider reads ~/.aider.conf.yml plus ~/.env and ~/.aider.env from base
// dir (default ~).
func ImportAider(profileDir, profile, sourceHome string) (Result, error) {
	base := sourceHome
	if base == "" {
		base = homeDir()
	}
	found := map[string]string{}
	if data, ok := loadYAMLMap(filepath.Join(base, ".aider.conf.yml")); ok {
		for yk, env := range aiderYAMLKeyMap {
			val := strings.TrimSpace(asString(data[yk]))
			if val != "" {
				if _, dup := found[env]; !dup {
					found[env] = val
				}
			}
		}
		for _, entry := range asSliceAny(data["api-key"]) {
			s := strings.TrimSpace(asString(entry))
			if i := strings.Index(s, "="); i != -1 {
				prefix := strings.ToLower(strings.TrimSpace(s[:i]))
				val := strings.TrimSpace(s[i+1:])
				env := aiderAPIKeyPrefixMap[prefix]
				if val != "" && env != "" {
					if _, dup := found[env]; !dup {
						found[env] = val
					}
				}
			}
		}
	}
	for _, name := range []string{".env", ".aider.env"} {
		dotenv := filepath.Join(base, name)
		if !exists(dotenv) {
			continue
		}
		vals := readDotenv(dotenv)
		for _, env := range aiderDotenvKeys {
			val := strings.TrimSpace(vals[env])
			if val != "" {
				if _, dup := found[env]; !dup {
					found[env] = val
				}
			}
		}
	}
	return finishMulti(profileDir, profile, found)
}

// ---------------------------------------------------------------------------
// shared finish for multi-key importers
// ---------------------------------------------------------------------------

func finishMulti(profileDir, profile string, found map[string]string) (Result, error) {
	if len(found) == 0 {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	providers, err := writeKeys(envFileFor(profileDir), found)
	if err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "api_keys", Providers: providers}, nil
}

// ---------------------------------------------------------------------------
// AWS (port of controller._detect_aws_credentials + import_aws_into_profile)
// ---------------------------------------------------------------------------

// readINISection parses one [section] of an INI file into a map with keys
// lower-cased, matching Python's configparser (which case-folds option names).
func readINISection(path, section string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	cur := ""
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if cur != section {
			continue
		}
		if i := strings.Index(line, "="); i != -1 {
			k := strings.ToLower(strings.TrimSpace(line[:i]))
			out[k] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

// ImportAWS writes AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY (+ optional
// AWS_SESSION_TOKEN, AWS_DEFAULT_REGION) from the environment or, failing that,
// the [default] profile of <awsDir>/credentials and <awsDir>/config.
// Default awsDir is ~/.aws.
func ImportAWS(profileDir, profile, sourceHome string) (Result, error) {
	accessKey := envKey("AWS_ACCESS_KEY_ID")
	secretKey := envKey("AWS_SECRET_ACCESS_KEY")
	sessionToken := envKey("AWS_SESSION_TOKEN")
	region := envKey("AWS_DEFAULT_REGION")

	awsDir := sourceHome
	if awsDir == "" {
		awsDir = filepath.Join(homeDir(), ".aws")
	}
	credsFile := filepath.Join(awsDir, "credentials")
	configFile := filepath.Join(awsDir, "config")
	source := ""
	if accessKey == "" && exists(credsFile) {
		creds := readINISection(credsFile, "default")
		accessKey = strings.TrimSpace(creds["aws_access_key_id"])
		secretKey = strings.TrimSpace(creds["aws_secret_access_key"])
		sessionToken = strings.TrimSpace(creds["aws_session_token"])
		if accessKey != "" {
			source = credsFile
		}
	}
	if region == "" && exists(configFile) {
		region = strings.TrimSpace(readINISection(configFile, "default")["region"])
	}
	if accessKey == "" || secretKey == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	env := envFileFor(profileDir)
	if err := UpsertEnvLine(env, "AWS_ACCESS_KEY_ID", accessKey); err != nil {
		return Result{}, err
	}
	if err := UpsertEnvLine(env, "AWS_SECRET_ACCESS_KEY", secretKey); err != nil {
		return Result{}, err
	}
	if sessionToken != "" {
		if err := UpsertEnvLine(env, "AWS_SESSION_TOKEN", sessionToken); err != nil {
			return Result{}, err
		}
	}
	if region != "" {
		if err := UpsertEnvLine(env, "AWS_DEFAULT_REGION", region); err != nil {
			return Result{}, err
		}
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "access_key", Provider: "aws-bedrock", Source: source}, nil
}

// ---------------------------------------------------------------------------
// Cursor (port of controller._detect_cursor_credentials)
// ---------------------------------------------------------------------------

var cursorKnownKeyMap = map[string]string{
	"openai.apiKey":          "OPENAI_API_KEY",
	"cursor.openaiApiKey":    "OPENAI_API_KEY",
	"anthropic.apiKey":       "ANTHROPIC_API_KEY",
	"cursor.anthropicApiKey": "ANTHROPIC_API_KEY",
	"gemini.apiKey":          "GEMINI_API_KEY",
	"cursor.googleApiKey":    "GEMINI_API_KEY",
}

var cursorValuePrefixes = []struct{ prefix, env string }{
	{"sk-ant-", "ANTHROPIC_API_KEY"},
	{"sk-or-", "OPENROUTER_API_KEY"},
	{"AIza", "GEMINI_API_KEY"},
}

// ImportCursor reads BYOK keys from Cursor's SQLite state.vscdb (opened
// read-only). If sourceHome is set it looks in <sourceHome>/state.vscdb,
// otherwise the platform default locations. All DB errors degrade to
// skipped-no-auth.
func ImportCursor(profileDir, profile, sourceHome string) (Result, error) {
	var candidates []string
	if sourceHome != "" {
		candidates = []string{filepath.Join(sourceHome, "state.vscdb")}
	} else {
		h := homeDir()
		candidates = []string{
			filepath.Join(h, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"),
			filepath.Join(h, ".config", "Cursor", "User", "globalStorage", "state.vscdb"),
		}
	}
	dbPath := ""
	for _, c := range candidates {
		if exists(c) {
			dbPath = c
			break
		}
	}
	if dbPath == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	found := map[string]string{}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err == nil {
		defer db.Close()
		rows, qerr := db.Query("SELECT key, value FROM ItemTable")
		if qerr == nil {
			defer rows.Close()
			for rows.Next() {
				var k, v string
				if rows.Scan(&k, &v) != nil {
					continue
				}
				value := strings.TrimSpace(v)
				if value == "" {
					continue
				}
				if env, ok := cursorKnownKeyMap[k]; ok {
					if _, dup := found[env]; !dup {
						found[env] = value
						continue
					}
				}
				for _, p := range cursorValuePrefixes {
					if strings.HasPrefix(value, p.prefix) {
						if _, dup := found[p.env]; !dup {
							found[p.env] = value
						}
						break
					}
				}
			}
		}
	}
	return finishMulti(profileDir, profile, found)
}

// ---------------------------------------------------------------------------
// Supermemory (port of cmd/import_._detect_supermemory_credentials)
// ---------------------------------------------------------------------------

// ImportSupermemory reads SUPERMEMORY_API_KEY from ~/.config/supermemory or
// ~/.supermemory config.json (api_key/token), else the environment, then also
// writes SUPERMEMORY_SESSION_INGEST=1. If sourceHome is set its config.json is
// tried first.
func ImportSupermemory(profileDir, profile, sourceHome string) (Result, error) {
	var candidates []string
	if sourceHome != "" {
		candidates = append(candidates, filepath.Join(sourceHome, "config.json"))
	}
	h := homeDir()
	candidates = append(candidates,
		filepath.Join(h, ".config", "supermemory", "config.json"),
		filepath.Join(h, ".supermemory", "config.json"),
	)
	key := ""
	for _, p := range candidates {
		if data, ok := loadJSONMap(p); ok {
			key = strings.TrimSpace(firstStr(data, "api_key", "token"))
			if key != "" {
				break
			}
		}
	}
	if key == "" {
		key = envKey("SUPERMEMORY_API_KEY")
	}
	if key == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	env := envFileFor(profileDir)
	if err := UpsertEnvLine(env, "SUPERMEMORY_API_KEY", key); err != nil {
		return Result{}, err
	}
	if err := UpsertEnvLine(env, "SUPERMEMORY_SESSION_INGEST", "1"); err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "supermemory"}, nil
}

// ---------------------------------------------------------------------------
// Honcho (port of cmd/import_._detect_honcho_credentials)
// ---------------------------------------------------------------------------

// ImportHoncho reads HONCHO_API_KEY (+ optional HONCHO_BASE_URL) from
// ~/.honcho/.env or ~/.config/honcho/config.yaml, else the environment. A base
// URL alone is configuration, not a credential, so an API key is required.
// If sourceHome is set, its .env and config.yaml are tried first.
func ImportHoncho(profileDir, profile, sourceHome string) (Result, error) {
	var candidates []string
	if sourceHome != "" {
		candidates = append(candidates,
			filepath.Join(sourceHome, ".env"),
			filepath.Join(sourceHome, "config.yaml"),
		)
	}
	h := homeDir()
	candidates = append(candidates,
		filepath.Join(h, ".honcho", ".env"),
		filepath.Join(h, ".config", "honcho", "config.yaml"),
	)
	found := map[string]string{}
	for _, path := range candidates {
		if !exists(path) {
			continue
		}
		if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
			if data, ok := loadYAMLMap(path); ok {
				if k := firstStr(data, "api_key", "HONCHO_API_KEY"); k != "" {
					found["HONCHO_API_KEY"] = k
				}
				if u := firstStr(data, "base_url", "HONCHO_BASE_URL"); u != "" {
					found["HONCHO_BASE_URL"] = u
				}
			}
		} else {
			vals := readDotenv(path)
			for _, key := range []string{"HONCHO_API_KEY", "HONCHO_BASE_URL"} {
				if v := strings.TrimSpace(vals[key]); v != "" {
					found[key] = v
				}
			}
		}
	}
	for _, key := range []string{"HONCHO_API_KEY", "HONCHO_BASE_URL"} {
		if _, ok := found[key]; !ok {
			if v := envKey(key); v != "" {
				found[key] = v
			}
		}
	}
	if _, ok := found["HONCHO_API_KEY"]; !ok {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	providers, err := writeKeys(envFileFor(profileDir), found)
	if err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "honcho", Providers: providers}, nil
}

// ---------------------------------------------------------------------------
// Nous Portal (port of cmd/import_._detect_nous_portal_credentials, PRD-006)
// ---------------------------------------------------------------------------

// ImportNousPortal reads NOUS_PORTAL_API_KEY from ~/.config/nousresearch or
// ~/.nousresearch JSON configs (api_key/token/key), else the environment, and
// flips gateway.use_gateway=true in the profile's config.yaml. If sourceHome is
// set its portal.json/config.json are tried first.
func ImportNousPortal(profileDir, profile, sourceHome string) (Result, error) {
	var candidates []string
	if sourceHome != "" {
		candidates = append(candidates,
			filepath.Join(sourceHome, "portal.json"),
			filepath.Join(sourceHome, "config.json"),
		)
	}
	h := homeDir()
	candidates = append(candidates,
		filepath.Join(h, ".config", "nousresearch", "portal.json"),
		filepath.Join(h, ".nousresearch", "config.json"),
		filepath.Join(h, ".nousresearch", "portal.json"),
	)
	key := ""
	for _, p := range candidates {
		if data, ok := loadJSONMap(p); ok {
			if k := firstStr(data, "api_key", "token", "key"); k != "" {
				key = k
				break
			}
		}
	}
	if key == "" {
		key = envKey("NOUS_PORTAL_API_KEY")
	}
	if strings.TrimSpace(key) == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	if err := UpsertEnvLine(envFileFor(profileDir), "NOUS_PORTAL_API_KEY", key); err != nil {
		return Result{}, err
	}
	enableProfileGateway(profileDir)
	return Result{Profile: profile, Status: StatusImported, Mode: "api_key", Provider: "nous_portal"}, nil
}

// enableProfileGateway sets gateway.use_gateway=true in <profileDir>/config.yaml,
// best-effort (a missing/unparseable file is left untouched), mirroring the
// Python import_nous_portal_into_profile behavior.
func enableProfileGateway(profileDir string) {
	cfgFile := filepath.Join(profileDir, "config.yaml")
	data, ok := loadYAMLMap(cfgFile)
	if !ok {
		return
	}
	gw, _ := data["gateway"].(map[string]any)
	if gw == nil {
		gw = map[string]any{}
	}
	gw["use_gateway"] = true
	data["gateway"] = gw
	b, err := yaml.Marshal(data)
	if err != nil {
		return
	}
	_ = os.WriteFile(cfgFile, b, 0o644)
}

// ---------------------------------------------------------------------------
// Execution backends: Docker / SSH / Modal / Daytona (PRD-005)
// (ports of cmd/import_.import_{docker,ssh,modal,daytona}_into_profile)
// ---------------------------------------------------------------------------

var dockerImageRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:@-]*$`)

// ImportDocker configures the Docker execution backend. Presence of registry
// auths in <dockerDir>/config.json (default ~/.docker) signals Docker is set
// up; the image is taken from DOCKER_DEFAULT_IMAGE (validated against the same
// regex as Python) or defaults to ubuntu:22.04.
func ImportDocker(profileDir, profile, sourceHome string) (Result, error) {
	dockerDir := sourceHome
	if dockerDir == "" {
		dockerDir = filepath.Join(homeDir(), ".docker")
	}
	configFile := filepath.Join(dockerDir, "config.json")
	hasAuths := false
	if data, ok := loadJSONMap(configFile); ok {
		if auths, ok := data["auths"].(map[string]any); ok && len(auths) > 0 {
			hasAuths = true
		}
	}
	image := envKey("DOCKER_DEFAULT_IMAGE")
	if image == "" && !hasAuths {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	if image == "" {
		image = "ubuntu:22.04"
	}
	if !dockerImageRe.MatchString(image) {
		return Result{}, fmt.Errorf("invalid Docker image name: %q", image)
	}
	source := ""
	if hasAuths {
		source = configFile
	}
	if err := UpsertEnvLine(envFileFor(profileDir), "DOCKER_DEFAULT_IMAGE", image); err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "backend", Provider: "docker", Source: source}, nil
}

var sshHostRe = regexp.MustCompile(`^[a-zA-Z0-9.\-_\[\]:]+$`)

// ImportSSH configures the SSH execution backend. Connection details come from
// SSH_HOST/SSH_USER/SSH_KEY_FILE/SSH_PORT in the environment or, failing that,
// the first concrete Host block of <sshDir>/config (default ~/.ssh). Only the
// key-file PATH is imported — never private key material. Host is validated
// against the same shell-metacharacter-blocking regex as Python.
func ImportSSH(profileDir, profile, sourceHome string) (Result, error) {
	sshDir := sourceHome
	if sshDir == "" {
		sshDir = filepath.Join(homeDir(), ".ssh")
	}
	host := envKey("SSH_HOST")
	user := envKey("SSH_USER")
	keyFile := envKey("SSH_KEY_FILE")
	port := envKey("SSH_PORT")
	source := ""
	if host == "" {
		host, user, keyFile, port, source = parseSSHConfig(filepath.Join(sshDir, "config"), user, keyFile, port)
	}
	if host == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	if !sshHostRe.MatchString(strings.TrimSpace(host)) {
		return Result{}, fmt.Errorf("invalid SSH host %q: must contain only alphanumerics, dots, hyphens, underscores, brackets, and colons (no shell metacharacters)", host)
	}
	env := envFileFor(profileDir)
	keys := []string{}
	if err := UpsertEnvLine(env, "SSH_HOST", host); err != nil {
		return Result{}, err
	}
	keys = append(keys, "SSH_HOST")
	if user != "" {
		if err := UpsertEnvLine(env, "SSH_USER", user); err != nil {
			return Result{}, err
		}
		keys = append(keys, "SSH_USER")
	}
	if keyFile != "" {
		if strings.HasPrefix(keyFile, "~") {
			keyFile = filepath.Join(homeDir(), keyFile[1:])
		}
		if err := UpsertEnvLine(env, "SSH_KEY_FILE", keyFile); err != nil {
			return Result{}, err
		}
		keys = append(keys, "SSH_KEY_FILE")
	}
	if port != "" && port != "22" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Result{}, fmt.Errorf("invalid SSH port %s: must be 1-65535", port)
		}
		if err := UpsertEnvLine(env, "SSH_PORT", port); err != nil {
			return Result{}, err
		}
		keys = append(keys, "SSH_PORT")
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "backend", Provider: "ssh", Providers: keys, Source: source}, nil
}

// parseSSHConfig extracts the first concrete Host block's connection details
// (skipping a bare wildcard "*"). Pre-seeded user/keyFile/port env values are
// preserved and not overwritten.
func parseSSHConfig(path, user, keyFile, port string) (h, u, kf, p, source string) {
	u, kf, p = user, keyFile, port
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	inHost := false
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val := strings.Join(fields[1:], " ")
		switch strings.ToLower(fields[0]) {
		case "host":
			if h != "" {
				return
			}
			if fields[1] == "*" {
				continue
			}
			inHost = true
			h = fields[1]
			source = path
		case "hostname":
			if inHost {
				h = fields[1]
			}
		case "user":
			if inHost && u == "" {
				u = val
			}
		case "identityfile":
			if inHost && kf == "" {
				kf = fields[1]
			}
		case "port":
			if inHost && p == "" {
				p = fields[1]
			}
		}
	}
	return
}

// ImportModal configures the Modal execution backend. MODAL_TOKEN_ID and
// MODAL_TOKEN_SECRET come from the environment or, failing that, ~/.modal.toml
// (token_id / token_secret). Both are required. If sourceHome is set it is used
// as the .modal.toml path directly.
func ImportModal(profileDir, profile, sourceHome string) (Result, error) {
	tokenID := envKey("MODAL_TOKEN_ID")
	tokenSecret := envKey("MODAL_TOKEN_SECRET")
	source := ""
	if tokenID == "" || tokenSecret == "" {
		tomlPath := sourceHome
		if tomlPath == "" {
			tomlPath = filepath.Join(homeDir(), ".modal.toml")
		}
		id, secret := parseModalToml(tomlPath)
		if tokenID == "" {
			tokenID = id
		}
		if tokenSecret == "" {
			tokenSecret = secret
		}
		if id != "" || secret != "" {
			source = tomlPath
		}
	}
	if strings.TrimSpace(tokenID) == "" || strings.TrimSpace(tokenSecret) == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	env := envFileFor(profileDir)
	if err := UpsertEnvLine(env, "MODAL_TOKEN_ID", tokenID); err != nil {
		return Result{}, err
	}
	if err := UpsertEnvLine(env, "MODAL_TOKEN_SECRET", tokenSecret); err != nil {
		return Result{}, err
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "backend", Provider: "modal", Source: source}, nil
}

// parseModalToml pulls token_id / token_secret out of a ~/.modal.toml file
// (first occurrence wins) without a full TOML dependency.
func parseModalToml(path string) (tokenID, tokenSecret string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		i := strings.Index(line, "=")
		if i == -1 {
			continue
		}
		v := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		switch strings.ToLower(strings.TrimSpace(line[:i])) {
		case "token_id":
			if tokenID == "" {
				tokenID = v
			}
		case "token_secret":
			if tokenSecret == "" {
				tokenSecret = v
			}
		}
	}
	return
}

// ImportDaytona configures the Daytona execution backend. DAYTONA_API_KEY (the
// credential, required) and optional DAYTONA_WORKSPACE_ID come from the
// environment or ~/.config/daytona/config.json. If sourceHome is set its
// config.json is tried first.
func ImportDaytona(profileDir, profile, sourceHome string) (Result, error) {
	apiKey := envKey("DAYTONA_API_KEY")
	workspaceID := envKey("DAYTONA_WORKSPACE_ID")
	source := ""
	if apiKey == "" {
		var candidates []string
		if sourceHome != "" {
			candidates = append(candidates, filepath.Join(sourceHome, "config.json"))
		}
		candidates = append(candidates, filepath.Join(homeDir(), ".config", "daytona", "config.json"))
		for _, p := range candidates {
			if data, ok := loadJSONMap(p); ok {
				if k := firstStr(data, "api_key", "apiKey", "token"); k != "" {
					apiKey = k
					if workspaceID == "" {
						workspaceID = firstStr(data, "workspace_id", "workspaceId")
					}
					source = p
					break
				}
			}
		}
	}
	if strings.TrimSpace(apiKey) == "" {
		return Result{Profile: profile, Status: StatusSkipped}, nil
	}
	env := envFileFor(profileDir)
	keys := []string{}
	if err := UpsertEnvLine(env, "DAYTONA_API_KEY", apiKey); err != nil {
		return Result{}, err
	}
	keys = append(keys, "DAYTONA_API_KEY")
	if workspaceID != "" {
		if err := UpsertEnvLine(env, "DAYTONA_WORKSPACE_ID", workspaceID); err != nil {
			return Result{}, err
		}
		keys = append(keys, "DAYTONA_WORKSPACE_ID")
	}
	return Result{Profile: profile, Status: StatusImported, Mode: "backend", Provider: "daytona", Providers: keys, Source: source}, nil
}

// ---------------------------------------------------------------------------
// small typed accessors + JSONC/YAML loading
// ---------------------------------------------------------------------------

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asString(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func nestedStr(m map[string]any, a, b string) string {
	inner, ok := m[a].(map[string]any)
	if !ok {
		return ""
	}
	return asString(inner[b])
}

func asSliceAny(v any) []any {
	s, _ := v.([]any)
	return s
}

func loadJSONMap(path string) (map[string]any, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var data map[string]any
	if json.Unmarshal(b, &data) != nil {
		return nil, false
	}
	return data, true
}

func loadYAMLMap(path string) (map[string]any, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var data map[string]any
	if yaml.Unmarshal(b, &data) != nil || data == nil {
		return nil, false
	}
	return data, true
}

var trailingCommaRe = regexp.MustCompile(`,(\s*[}\]])`)

// stripJSONC removes // and /* */ comments and trailing commas. Port of
// controller._strip_jsonc.
func stripJSONC(text string) string {
	var out strings.Builder
	i, n := 0, len(text)
	inString := false
	for i < n {
		ch := text[i]
		if inString {
			out.WriteByte(ch)
			if ch == '\\' && i+1 < n {
				out.WriteByte(text[i+1])
				i += 2
				continue
			}
			if ch == '"' {
				inString = false
			}
			i++
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			i++
			continue
		}
		if ch == '/' && i+1 < n && text[i+1] == '/' {
			i += 2
			for i < n && text[i] != '\r' && text[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < n && text[i+1] == '*' {
			i += 2
			for i+1 < n && !(text[i] == '*' && text[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		out.WriteByte(ch)
		i++
	}
	return trailingCommaRe.ReplaceAllString(out.String(), "$1")
}

func loadJSONC(b []byte) (map[string]any, bool) {
	var data map[string]any
	if json.Unmarshal(b, &data) == nil {
		return data, true
	}
	if json.Unmarshal([]byte(stripJSONC(string(b))), &data) == nil {
		return data, true
	}
	return nil, false
}
