#!/usr/bin/env node
// Builds the npm packages from GoReleaser's dist/ output.
//
//   node scripts/npm-packages.mjs <version> [dist-dir] [out-dir]
//
// Writes one directory per package to out-dir (default npm/dist): the four
// platform packages, each holding one native binary, and the main `prwatch`
// package, whose optionalDependencies pin them to the same version.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const [version, distDir = path.join(root, "dist"), outDir = path.join(root, "npm", "dist")] = process.argv.slice(2);
if (!version || !/^\d+\.\d+\.\d+(-[\w.]+)?$/.test(version)) {
  console.error("usage: npm-packages.mjs <version> [dist-dir] [out-dir]");
  process.exit(2);
}

const goToNode = { amd64: "x64", arm64: "arm64" };
const artifacts = JSON.parse(fs.readFileSync(path.join(distDir, "artifacts.json"), "utf8"));
const binaries = artifacts.filter((a) => a.type === "Binary" && a.name === "prwatch");
const repository = { type: "git", url: "git+https://github.com/tvdavies/prwatch.git" };
const licence = fs.readFileSync(path.join(root, "LICENSE"));

fs.rmSync(outDir, { recursive: true, force: true });
const optionalDependencies = {};
for (const b of binaries) {
  const os = b.goos;
  const cpu = goToNode[b.goarch];
  if (!cpu) throw new Error(`unexpected arch ${b.goarch}`);
  const name = `@tvdavies/prwatch-${os}-${cpu}`;
  const dir = path.join(outDir, `prwatch-${os}-${cpu}`);
  fs.mkdirSync(path.join(dir, "bin"), { recursive: true });
  fs.copyFileSync(path.resolve(root, b.path), path.join(dir, "bin", "prwatch"));
  fs.chmodSync(path.join(dir, "bin", "prwatch"), 0o755);
  fs.writeFileSync(path.join(dir, "LICENSE"), licence);
  fs.writeFileSync(
    path.join(dir, "README.md"),
    `# ${name}\n\nThe ${os}/${cpu} binary for [prwatch](https://github.com/tvdavies/prwatch). Install \`prwatch\` instead.\n`,
  );
  const pkg = {
    name,
    version,
    description: `prwatch binary for ${os} ${cpu}`,
    license: "MIT",
    repository,
    os: [os],
    cpu: [cpu],
    files: ["bin/prwatch"],
    preferUnplugged: true,
    publishConfig: { access: "public" },
  };
  fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify(pkg, null, 2) + "\n");
  optionalDependencies[name] = version;
}
if (Object.keys(optionalDependencies).length !== 4) {
  throw new Error(`expected 4 binaries, found ${Object.keys(optionalDependencies).length}`);
}

const mainSrc = path.join(root, "npm", "prwatch");
const mainDir = path.join(outDir, "prwatch");
fs.cpSync(mainSrc, mainDir, { recursive: true });
fs.copyFileSync(path.join(root, "LICENSE"), path.join(mainDir, "LICENSE"));
fs.copyFileSync(path.join(root, "README.md"), path.join(mainDir, "README.md"));
const main = JSON.parse(fs.readFileSync(path.join(mainSrc, "package.json"), "utf8"));
main.version = version;
main.optionalDependencies = Object.fromEntries(Object.entries(optionalDependencies).sort());
main.publishConfig = { access: "public" };
fs.writeFileSync(path.join(mainDir, "package.json"), JSON.stringify(main, null, 2) + "\n");

// Platform packages first: the main package depends on them.
const order = [...Object.keys(optionalDependencies).sort().map((n) => `prwatch-${n.split("prwatch-")[1]}`), "prwatch"];
fs.writeFileSync(path.join(outDir, "publish-order.txt"), order.join("\n") + "\n");
console.log(`wrote ${order.length} packages for ${version} to ${path.relative(root, outDir)}`);
