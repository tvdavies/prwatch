"use strict";

// Postinstall: replace bin/prwatch (a JS launcher) with the native binary
// from the matching platform package, as esbuild does. Running `prwatch`
// then execs the Go binary directly, with no Node parent process. Any
// failure leaves the JS launcher in place, which still works.

const fs = require("node:fs");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const platforms = require("./platforms.js");

function optimise() {
  if (process.platform === "win32") return;
  // Yarn may run install scripts repeatedly and does not support binary bin
  // files well; keep the launcher there.
  if (/\byarn\//.test(process.env.npm_config_user_agent || "")) return;
  const pkg = platforms[`${process.platform} ${process.arch}`];
  if (!pkg) return;
  let bin;
  try {
    bin = require.resolve(`${pkg}/bin/prwatch`);
  } catch {
    return;
  }
  try {
    fs.chmodSync(bin, 0o755);
  } catch {
    // Read-only stores already have the right mode.
  }
  try {
    execFileSync(bin, ["version"], { stdio: "ignore", timeout: 10000 });
  } catch {
    return;
  }
  const target = path.join(__dirname, "bin", "prwatch");
  const tmp = path.join(__dirname, "bin", ".prwatch-native");
  try {
    fs.unlinkSync(tmp);
  } catch {}
  try {
    fs.linkSync(bin, tmp);
  } catch {
    try {
      fs.copyFileSync(bin, tmp);
      fs.chmodSync(tmp, 0o755);
    } catch {
      return;
    }
  }
  try {
    fs.renameSync(tmp, target);
  } catch {}
  try {
    fs.unlinkSync(tmp);
  } catch {}
}

try {
  optimise();
} catch {
  // Never fail the install over an optimisation.
}
