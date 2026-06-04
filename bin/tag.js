#!/usr/bin/env node

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const PACKAGE_ROOT = path.resolve(__dirname, "..");
const PACKAGE_JSON = require(path.join(PACKAGE_ROOT, "package.json"));
const VERSION = PACKAGE_JSON.version;
const RUNTIME_HOME =
  process.env.TAG_NPM_RUNTIME_HOME ||
  path.join(os.homedir(), ".tag", "npm-runtime", VERSION);
const VENV_DIR = path.join(RUNTIME_HOME, "venv");
const STAMP_FILE = path.join(VENV_DIR, ".tag-package-version");
const FORCE_REINSTALL =
  process.env.TAG_NPM_FORCE_REINSTALL === "1" ||
  process.argv.includes("--reinstall-runtime");

function fail(message, code = 1) {
  console.error(message);
  process.exit(code);
}

function run(cmd, args, options = {}) {
  const result = spawnSync(cmd, args, {
    stdio: "inherit",
    env: process.env,
    ...options,
  });
  if (result.error) {
    throw result.error;
  }
  if (result.status !== 0) {
    const rendered = [cmd, ...args].join(" ");
    fail(`TAG npm launcher failed while running: ${rendered}`, result.status || 1);
  }
}

function detectPython() {
  const candidates =
    process.platform === "win32"
      ? [
          { cmd: "py", launcherArgs: ["-3.13"] },
          { cmd: "py", launcherArgs: ["-3.12"] },
          { cmd: "py", launcherArgs: ["-3.11"] },
          { cmd: "py", launcherArgs: ["-3"] },
          { cmd: "python" },
          { cmd: "python3" },
        ]
      : [
          { cmd: "python3.13" },
          { cmd: "python3.12" },
          { cmd: "python3.11" },
          { cmd: "python3" },
          { cmd: "python" },
        ];
  for (const candidate of candidates) {
    const args = [...(candidate.launcherArgs || []), "--version"];
    const probe = spawnSync(candidate.cmd, args, { encoding: "utf8" });
    if (probe.status === 0) {
      return { cmd: candidate.cmd, launcherArgs: candidate.launcherArgs || [] };
    }
  }
  return null;
}

function pythonVersionOk(python) {
  const args = [...python.launcherArgs, "-c", "import sys; print(f'{sys.version_info[0]}.{sys.version_info[1]}')"];
  const probe = spawnSync(python.cmd, args, { encoding: "utf8" });
  if (probe.status !== 0) {
    return false;
  }
  const parts = String(probe.stdout || "").trim().split(".");
  const major = Number(parts[0] || 0);
  const minor = Number(parts[1] || 0);
  return major === 3 && minor >= 11 && minor < 14;
}

function venvTagBinary() {
  if (process.platform === "win32") {
    return path.join(VENV_DIR, "Scripts", "tag.exe");
  }
  return path.join(VENV_DIR, "bin", "tag");
}

function venvPythonBinary() {
  if (process.platform === "win32") {
    return path.join(VENV_DIR, "Scripts", "python.exe");
  }
  return path.join(VENV_DIR, "bin", "python");
}

function readStamp() {
  try {
    return fs.readFileSync(STAMP_FILE, "utf8").trim();
  } catch {
    return "";
  }
}

function writeStamp() {
  fs.mkdirSync(path.dirname(STAMP_FILE), { recursive: true });
  fs.writeFileSync(STAMP_FILE, `${VERSION}\n`, "utf8");
}

function ensureRuntime() {
  const python = detectPython();
  if (!python) {
    fail("TAG requires Python 3.11+ to be available on PATH for the npm launcher.");
  }
  if (!pythonVersionOk(python)) {
    fail("TAG requires Python >=3.11 and <3.14 for the npm launcher.");
  }

  const tagBin = venvTagBinary();
  const stamp = readStamp();
  if (!FORCE_REINSTALL && fs.existsSync(tagBin) && stamp === VERSION) {
    return tagBin;
  }

  fs.mkdirSync(RUNTIME_HOME, { recursive: true });
  run(python.cmd, [...python.launcherArgs, "-m", "venv", VENV_DIR]);

  const venvPython = venvPythonBinary();
  run(venvPython, ["-m", "ensurepip", "--upgrade"]);
  run(venvPython, ["-m", "pip", "install", "--upgrade", "pip"]);
  run(venvPython, ["-m", "pip", "install", PACKAGE_ROOT]);
  writeStamp();
  return venvTagBinary();
}

function main() {
  try {
    const tagBin = ensureRuntime();
    const forwardedArgs = process.argv.slice(2).filter((arg) => arg !== "--reinstall-runtime");
    const result = spawnSync(tagBin, forwardedArgs, {
      stdio: "inherit",
      env: process.env,
    });
    if (result.error) {
      throw result.error;
    }
    process.exit(result.status || 0);
  } catch (error) {
    fail(`TAG npm launcher error: ${error.message || String(error)}`);
  }
}

main();
