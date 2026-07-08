package importer

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// clearCredEnv unsets every provider env var so a stray value in the test
// runner's environment can't leak into a detection path.
func clearCredEnv(t *testing.T) {
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "MISTRAL_API_KEY",
		"GITHUB_TOKEN", "GH_TOKEN", "TAG_IMPORT_CODEX_HOME", "OPENROUTER_API_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_DEFAULT_REGION",
		"SUPERMEMORY_API_KEY", "HONCHO_API_KEY", "HONCHO_BASE_URL", "NOUS_PORTAL_API_KEY",
		"DOCKER_DEFAULT_IMAGE", "SSH_HOST", "SSH_USER", "SSH_KEY_FILE", "SSH_PORT",
		"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "DAYTONA_API_KEY", "DAYTONA_WORKSPACE_ID",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func profileDir(t *testing.T) string {
	d := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func readEnv(t *testing.T, dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	return string(b)
}

func assertEnvContains(t *testing.T, dir, want string) {
	t.Helper()
	got := readEnv(t, dir)
	if !strings.Contains(got, want) {
		t.Fatalf(".env missing %q; got:\n%s", want, got)
	}
	info, err := os.Stat(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf(".env mode = %o, want 0600", perm)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertEnvLine(t *testing.T) {
	dir := profileDir(t)
	env := filepath.Join(dir, ".env")
	if err := UpsertEnvLine(env, "FOO", "1"); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnvLine(env, "BAR", "2"); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEnvLine(env, "FOO", "3"); err != nil { // update, not clobber
		t.Fatal(err)
	}
	got := readEnv(t, dir)
	if !strings.Contains(got, "FOO=3") || !strings.Contains(got, "BAR=2") {
		t.Fatalf("unexpected .env:\n%s", got)
	}
	if strings.Count(got, "FOO=") != 1 {
		t.Fatalf("FOO duplicated:\n%s", got)
	}
	info, _ := os.Stat(env)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestImportCodex(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "codex")
	writeFile(t, filepath.Join(src, "auth.json"), `{"OPENAI_API_KEY":"sk-codex-123"}`)
	dir := profileDir(t)
	res, err := ImportCodex(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "OPENAI_API_KEY=sk-codex-123")
}

func TestImportCodexMissing(t *testing.T) {
	clearCredEnv(t)
	res, err := ImportCodex(profileDir(t), "p", filepath.Join(t.TempDir(), "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s, want skipped", res.Status)
	}
}

func TestImportClaudeAPIKey(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-xyz")
	dir := profileDir(t)
	res, err := ImportClaude(dir, "p", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "api_key" {
		t.Fatalf("mode = %s", res.Mode)
	}
	assertEnvContains(t, dir, "ANTHROPIC_API_KEY=sk-ant-xyz")
}

func TestImportClaudeOAuth(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "claude")
	writeFile(t, filepath.Join(src, ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"oauth-tok-1"}}`)
	dir := profileDir(t)
	res, err := ImportClaude(dir, "p", src, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "oauth" || res.TOSWarn == "" {
		t.Fatalf("unexpected result %+v", res)
	}
	assertEnvContains(t, dir, "CLAUDE_CODE_OAUTH_TOKEN=oauth-tok-1")
}

func TestImportClaudeMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportClaude(profileDir(t), "p", filepath.Join(t.TempDir(), "none"), false)
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestImportGeminiEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("GEMINI_API_KEY", "gm-123")
	dir := profileDir(t)
	if _, err := ImportGemini(dir, "p", "", false); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "GEMINI_API_KEY=gm-123")
}

func TestImportGeminiDotenv(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "gemini")
	writeFile(t, filepath.Join(src, ".env"), "GEMINI_API_KEY=gm-file-9\n")
	dir := profileDir(t)
	if _, err := ImportGemini(dir, "p", src, false); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "GEMINI_API_KEY=gm-file-9")
}

func TestImportContinueYAML(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "continue")
	writeFile(t, filepath.Join(src, "config.yaml"), `
models:
  - provider: anthropic
    apiKey: sk-ant-cont
  - provider: openai
    apiKey: sk-oai-cont
`)
	dir := profileDir(t)
	res, err := ImportContinue(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "ANTHROPIC_API_KEY=sk-ant-cont")
	assertEnvContains(t, dir, "OPENAI_API_KEY=sk-oai-cont")
}

func TestImportContinueLocalEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("MY_OAI", "sk-from-env")
	src := filepath.Join(t.TempDir(), "continue")
	writeFile(t, filepath.Join(src, "config.json"),
		`{"models":[{"provider":"openai","apiKey":"localEnv:MY_OAI"}]}`)
	dir := profileDir(t)
	if _, err := ImportContinue(dir, "p", src); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "OPENAI_API_KEY=sk-from-env")
}

func TestImportMistral(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "vibe")
	writeFile(t, filepath.Join(src, ".env"), "MISTRAL_API_KEY=ms-9\n")
	dir := profileDir(t)
	if _, err := ImportMistral(dir, "p", src); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "MISTRAL_API_KEY=ms-9")
}

func TestImportMistralMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportMistral(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestImportOpencode(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "opencode")
	writeFile(t, filepath.Join(src, "auth.json"),
		`{"anthropic":{"type":"api","key":"sk-ant-oc"},"ollama":{"type":"oauth"}}`)
	dir := profileDir(t)
	res, err := ImportOpencode(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "ANTHROPIC_API_KEY=sk-ant-oc")
}

func TestImportZedJSONC(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "zed", "settings.json")
	writeFile(t, src, `{
  // trailing comma + comment
  "language_models": {
    "anthropic": { "api_key": "sk-ant-zed", },
  },
}`)
	dir := profileDir(t)
	res, err := ImportZed(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "ANTHROPIC_API_KEY=sk-ant-zed")
}

func TestImportCopilotEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_env")
	dir := profileDir(t)
	if _, err := ImportCopilot(dir, "p", ""); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "GITHUB_TOKEN=ghp_env")
}

func TestImportCopilotHosts(t *testing.T) {
	clearCredEnv(t)
	hosts := filepath.Join(t.TempDir(), "hosts.yml")
	writeFile(t, hosts, "github.com:\n  oauth_token: ghp_hosts\n")
	dir := profileDir(t)
	if _, err := ImportCopilot(dir, "p", hosts); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "GITHUB_TOKEN=ghp_hosts")
}

func TestImportCopilotMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportCopilot(profileDir(t), "p", filepath.Join(t.TempDir(), "none.yml"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

func TestImportAiderYAML(t *testing.T) {
	clearCredEnv(t)
	base := t.TempDir()
	writeFile(t, filepath.Join(base, ".aider.conf.yml"),
		"anthropic-api-key: sk-ant-aider\napi-key:\n  - gemini=sk-gm-aider\n")
	dir := profileDir(t)
	res, err := ImportAider(dir, "p", base)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "ANTHROPIC_API_KEY=sk-ant-aider")
	assertEnvContains(t, dir, "GEMINI_API_KEY=sk-gm-aider")
}

func TestImportAiderDotenv(t *testing.T) {
	clearCredEnv(t)
	base := t.TempDir()
	writeFile(t, filepath.Join(base, ".env"), "GEMINI_API_KEY=gm-aider\n")
	dir := profileDir(t)
	if _, err := ImportAider(dir, "p", base); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "GEMINI_API_KEY=gm-aider")
}

func TestImportAiderMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportAider(profileDir(t), "p", t.TempDir())
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// AWS
// ---------------------------------------------------------------------------

func TestImportAWSEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-env")
	t.Setenv("AWS_DEFAULT_REGION", "us-west-2")
	dir := profileDir(t)
	res, err := ImportAWS(dir, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "access_key" {
		t.Fatalf("mode = %s", res.Mode)
	}
	assertEnvContains(t, dir, "AWS_ACCESS_KEY_ID=AKIAENV")
	assertEnvContains(t, dir, "AWS_SECRET_ACCESS_KEY=secret-env")
	assertEnvContains(t, dir, "AWS_DEFAULT_REGION=us-west-2")
}

func TestImportAWSFile(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "aws")
	writeFile(t, filepath.Join(src, "credentials"),
		"[default]\naws_access_key_id = AKIAFILE\naws_secret_access_key = secret-file\naws_session_token = tok-file\n")
	writeFile(t, filepath.Join(src, "config"), "[default]\nregion = eu-central-1\n")
	dir := profileDir(t)
	if _, err := ImportAWS(dir, "p", src); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "AWS_ACCESS_KEY_ID=AKIAFILE")
	assertEnvContains(t, dir, "AWS_SECRET_ACCESS_KEY=secret-file")
	assertEnvContains(t, dir, "AWS_SESSION_TOKEN=tok-file")
	assertEnvContains(t, dir, "AWS_DEFAULT_REGION=eu-central-1")
}

func TestImportAWSMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportAWS(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Cursor (SQLite state.vscdb)
// ---------------------------------------------------------------------------

func makeCursorDB(t *testing.T, dir string, rows map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE ItemTable (key TEXT, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		if _, err := db.Exec("INSERT INTO ItemTable (key, value) VALUES (?, ?)", k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportCursor(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "cursor")
	makeCursorDB(t, src, map[string]string{
		"anthropic.apiKey": "sk-ant-cursor",
		"some.other.key":   "AIzaGeminiViaPrefix",
	})
	dir := profileDir(t)
	res, err := ImportCursor(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "ANTHROPIC_API_KEY=sk-ant-cursor")
	assertEnvContains(t, dir, "GEMINI_API_KEY=AIzaGeminiViaPrefix")
}

func TestImportCursorMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportCursor(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Supermemory
// ---------------------------------------------------------------------------

func TestImportSupermemoryFile(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "sm")
	writeFile(t, filepath.Join(src, "config.json"), `{"api_key":"sm-file-1"}`)
	dir := profileDir(t)
	res, err := ImportSupermemory(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "SUPERMEMORY_API_KEY=sm-file-1")
	assertEnvContains(t, dir, "SUPERMEMORY_SESSION_INGEST=1")
}

func TestImportSupermemoryEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("SUPERMEMORY_API_KEY", "sm-env-2")
	dir := profileDir(t)
	if _, err := ImportSupermemory(dir, "p", filepath.Join(t.TempDir(), "none")); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "SUPERMEMORY_API_KEY=sm-env-2")
}

func TestImportSupermemoryMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportSupermemory(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Honcho
// ---------------------------------------------------------------------------

func TestImportHonchoEnvFile(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "honcho")
	writeFile(t, filepath.Join(src, ".env"),
		"HONCHO_API_KEY=hon-file\nHONCHO_BASE_URL=https://honcho.example\n")
	dir := profileDir(t)
	res, err := ImportHoncho(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "HONCHO_API_KEY=hon-file")
	assertEnvContains(t, dir, "HONCHO_BASE_URL=https://honcho.example")
}

func TestImportHonchoYAML(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "honcho")
	writeFile(t, filepath.Join(src, "config.yaml"), "api_key: hon-yaml\nbase_url: https://h.example\n")
	dir := profileDir(t)
	if _, err := ImportHoncho(dir, "p", src); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "HONCHO_API_KEY=hon-yaml")
}

// A base URL alone is not a credential -> skipped-no-auth.
func TestImportHonchoBaseURLOnly(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "honcho")
	writeFile(t, filepath.Join(src, ".env"), "HONCHO_BASE_URL=https://only.example\n")
	res, err := ImportHoncho(profileDir(t), "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s, want skipped", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Nous Portal
// ---------------------------------------------------------------------------

func TestImportNousPortalFileAndGateway(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "nous")
	writeFile(t, filepath.Join(src, "portal.json"), `{"api_key":"nous-key-1234567890"}`)
	dir := profileDir(t)
	writeFile(t, filepath.Join(dir, "config.yaml"), "gateway:\n  use_gateway: false\n")
	res, err := ImportNousPortal(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "NOUS_PORTAL_API_KEY=nous-key-1234567890")
	cfg, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "use_gateway: true") {
		t.Fatalf("gateway not enabled:\n%s", cfg)
	}
}

func TestImportNousPortalMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportNousPortal(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Docker
// ---------------------------------------------------------------------------

func TestImportDockerConfigAuths(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "docker")
	writeFile(t, filepath.Join(src, "config.json"), `{"auths":{"registry.example":{"auth":"Zm9vOmJhcg=="}}}`)
	dir := profileDir(t)
	res, err := ImportDocker(dir, "p", src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	assertEnvContains(t, dir, "DOCKER_DEFAULT_IMAGE=ubuntu:22.04")
}

func TestImportDockerEnvImage(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("DOCKER_DEFAULT_IMAGE", "python:3.12")
	dir := profileDir(t)
	if _, err := ImportDocker(dir, "p", filepath.Join(t.TempDir(), "none")); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "DOCKER_DEFAULT_IMAGE=python:3.12")
}

func TestImportDockerInvalidImage(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("DOCKER_DEFAULT_IMAGE", "bad;rm -rf")
	_, err := ImportDocker(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if err == nil {
		t.Fatal("expected error for invalid image")
	}
}

func TestImportDockerMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportDocker(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// SSH
// ---------------------------------------------------------------------------

func TestImportSSHEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("SSH_HOST", "build.example.com")
	t.Setenv("SSH_USER", "runner")
	t.Setenv("SSH_KEY_FILE", "/keys/id_ed25519")
	t.Setenv("SSH_PORT", "2222")
	dir := profileDir(t)
	if _, err := ImportSSH(dir, "p", ""); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "SSH_HOST=build.example.com")
	assertEnvContains(t, dir, "SSH_USER=runner")
	assertEnvContains(t, dir, "SSH_KEY_FILE=/keys/id_ed25519")
	assertEnvContains(t, dir, "SSH_PORT=2222")
}

func TestImportSSHConfigFile(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "ssh")
	writeFile(t, filepath.Join(src, "config"),
		"Host *\n  ForwardAgent yes\n\nHost prod\n  HostName prod.internal\n  User deploy\n  IdentityFile ~/.ssh/prod_key\n  Port 2200\n")
	dir := profileDir(t)
	if _, err := ImportSSH(dir, "p", src); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "SSH_HOST=prod.internal")
	assertEnvContains(t, dir, "SSH_USER=deploy")
	assertEnvContains(t, dir, "SSH_PORT=2200")
}

func TestImportSSHInvalidHost(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("SSH_HOST", "evil;rm -rf /")
	_, err := ImportSSH(profileDir(t), "p", "")
	if err == nil {
		t.Fatal("expected error for invalid host")
	}
}

func TestImportSSHMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportSSH(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Modal
// ---------------------------------------------------------------------------

func TestImportModalEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("MODAL_TOKEN_ID", "ak-id")
	t.Setenv("MODAL_TOKEN_SECRET", "as-secret")
	dir := profileDir(t)
	if _, err := ImportModal(dir, "p", ""); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "MODAL_TOKEN_ID=ak-id")
	assertEnvContains(t, dir, "MODAL_TOKEN_SECRET=as-secret")
}

func TestImportModalToml(t *testing.T) {
	clearCredEnv(t)
	toml := filepath.Join(t.TempDir(), ".modal.toml")
	writeFile(t, toml, "[default]\ntoken_id = \"ak-file\"\ntoken_secret = \"as-file\"\nactive = true\n")
	dir := profileDir(t)
	if _, err := ImportModal(dir, "p", toml); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "MODAL_TOKEN_ID=ak-file")
	assertEnvContains(t, dir, "MODAL_TOKEN_SECRET=as-file")
}

func TestImportModalMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportModal(profileDir(t), "p", filepath.Join(t.TempDir(), "none.toml"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// ---------------------------------------------------------------------------
// Daytona
// ---------------------------------------------------------------------------

func TestImportDaytonaEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("DAYTONA_API_KEY", "dt-key")
	t.Setenv("DAYTONA_WORKSPACE_ID", "ws-1")
	dir := profileDir(t)
	if _, err := ImportDaytona(dir, "p", ""); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "DAYTONA_API_KEY=dt-key")
	assertEnvContains(t, dir, "DAYTONA_WORKSPACE_ID=ws-1")
}

func TestImportDaytonaFile(t *testing.T) {
	clearCredEnv(t)
	src := filepath.Join(t.TempDir(), "daytona")
	writeFile(t, filepath.Join(src, "config.json"), `{"api_key":"dt-file","workspace_id":"ws-file"}`)
	dir := profileDir(t)
	if _, err := ImportDaytona(dir, "p", src); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "DAYTONA_API_KEY=dt-file")
	assertEnvContains(t, dir, "DAYTONA_WORKSPACE_ID=ws-file")
}

func TestImportDaytonaMissing(t *testing.T) {
	clearCredEnv(t)
	res, _ := ImportDaytona(profileDir(t), "p", filepath.Join(t.TempDir(), "none"))
	if res.Status != StatusSkipped {
		t.Fatalf("status = %s", res.Status)
	}
}

// Ensure a pre-existing unrelated line survives an import (no clobber).
func TestImportPreservesExistingLines(t *testing.T) {
	clearCredEnv(t)
	t.Setenv("MISTRAL_API_KEY", "ms-new")
	dir := profileDir(t)
	writeFile(t, filepath.Join(dir, ".env"), "EXISTING=keep-me\n")
	if _, err := ImportMistral(dir, "p", ""); err != nil {
		t.Fatal(err)
	}
	assertEnvContains(t, dir, "EXISTING=keep-me")
	assertEnvContains(t, dir, "MISTRAL_API_KEY=ms-new")
}
