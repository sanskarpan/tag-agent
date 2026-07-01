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
// `--reinstall-runtime` is a launcher directive, not user data: honor it only
// as the leading argument so it can't collide with a downstream value the child
// legitimately receives (e.g. `tag submit --flag --reinstall-runtime`).
const FORCE_REINSTALL =
  process.env.TAG_NPM_FORCE_REINSTALL === "1" ||
  process.argv[2] === "--reinstall-runtime";

// Synchronous sleep with no dependencies — used to poll the install lock.
function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

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
  if (!FORCE_REINSTALL && fs.existsSync(tagBin) && readStamp() === VERSION) {
    return tagBin;
  }

  fs.mkdirSync(RUNTIME_HOME, { recursive: true });

  // Serialize concurrent cold first-runs with an advisory lock: two racing
  // `tag <cmd>` invocations must not both run `python -m venv` + pip install
  // into the same shared VENV_DIR, which corrupts the interpreter.
  const lockPath = path.join(RUNTIME_HOME, ".install.lock");
  const releaseLock = acquireInstallLock(lockPath);
  try {
    // Re-check under the lock: the process we waited on may have finished the
    // install, in which case there is nothing left to do.
    if (!FORCE_REINSTALL && fs.existsSync(tagBin) && readStamp() === VERSION) {
      return tagBin;
    }

    run(python.cmd, [...python.launcherArgs, "-m", "venv", VENV_DIR]);

    const venvPython = venvPythonBinary();
    run(venvPython, ["-m", "ensurepip", "--upgrade"]);
    run(venvPython, ["-m", "pip", "install", "--upgrade", "pip"]);
    run(venvPython, ["-m", "pip", "install", PACKAGE_ROOT]);
    writeStamp();
    return venvTagBinary();
  } finally {
    releaseLock();
  }
}

// Acquire an exclusive advisory lock via O_EXCL create, polling until free.
// Returns a release() that removes the lock. A stale lock older than the
// timeout is forcibly reclaimed so a crashed installer can't wedge every
// future invocation.
function acquireInstallLock(lockPath, timeoutMs = 600000) {
  const start = Date.now();
  while (true) {
    try {
      const fd = fs.openSync(lockPath, "wx");
      fs.writeSync(fd, String(process.pid));
      fs.closeSync(fd);
      return () => {
        try {
          fs.unlinkSync(lockPath);
        } catch {
          /* already gone */
        }
      };
    } catch (err) {
      if (err.code !== "EEXIST") {
        throw err;
      }
      let age = Infinity;
      try {
        age = Date.now() - fs.statSync(lockPath).mtimeMs;
      } catch {
        // Lock vanished between open and stat — retry immediately.
        continue;
      }
      if (age > timeoutMs || Date.now() - start > timeoutMs) {
        try {
          fs.unlinkSync(lockPath);
        } catch {
          /* another waiter reclaimed it */
        }
        continue;
      }
      sleepSync(250);
    }
  }
}

function main() {
  try {
    // Strip only a leading `--reinstall-runtime` (the launcher directive);
    // any later occurrence is genuine user data destined for the child.
    let forwardedArgs = process.argv.slice(2);
    if (forwardedArgs[0] === "--reinstall-runtime") {
      forwardedArgs = forwardedArgs.slice(1);
    }

    // Fast path: `--version` needs no Python runtime, so short-circuit before
    // ensureRuntime() to avoid triggering a multi-minute cold venv build just
    // to print a version string. Skip this fast path when a runtime reinstall
    // was requested (`--reinstall-runtime --version`) so the reinstall isn't
    // silently dropped — fall through to ensureRuntime(), then print version.
    if (
      !FORCE_REINSTALL &&
      forwardedArgs.length === 1 &&
      (forwardedArgs[0] === "--version" || forwardedArgs[0] === "-V")
    ) {
      process.stdout.write(`${VERSION}\n`);
      process.exit(0);
    }

    const tagBin = ensureRuntime();
    const result = spawnSync(tagBin, forwardedArgs, {
      stdio: "inherit",
      env: process.env,
    });
    if (result.error) {
      throw result.error;
    }
    // Signal death yields {status:null, signal:'SIGTERM'}: surface it as a
    // non-zero exit (128+signo, the shell convention) so CI/`set -e`/watchdogs
    // detect the failure instead of seeing a masked exit 0.
    if (result.signal) {
      const signo = os.constants.signals[result.signal];
      process.exit(signo ? 128 + signo : 1);
    }
    process.exit(result.status == null ? 1 : result.status);
  } catch (error) {
    fail(`TAG npm launcher error: ${error.message || String(error)}`);
  }
}

main();

