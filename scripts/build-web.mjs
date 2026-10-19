// Build the web console from src/ into dist/.
//
// There is NO bundler: the console is dependency-light vanilla JS that runs
// straight from <script> tags. "Building" means copying src/ to dist/ while
// substituting the __BUILD_*__ macros, so the checked-in dist/ stays usable
// by a plain `go build` on machines without Node.
//
// Uses only Node standard library. Run: node scripts/build-web.mjs

import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
// The root normally resolves from this script's location. REPO_ROOT lets the
// caller pin it when the script is executed from a copy (some sandboxes run
// scripts from a temp path, where `../web` would point at the wrong place).
const repoRoot = process.env.RELAYPOOL_REPO_ROOT
  ? process.env.RELAYPOOL_REPO_ROOT
  : join(here, "..");

const srcDir = join(repoRoot, "web", "src");
const outDir = join(repoRoot, "web", "dist");
const vendorDir = join(repoRoot, "web", "vendor");
const embedDir = join(repoRoot, "internal", "api", "console");
if(resolve(outDir)!==resolve(repoRoot,"web","dist") || resolve(embedDir)!==resolve(repoRoot,"internal","api","console"))throw new Error("构建目录越界");

// The version stamp is best effort: a source checkout without git, or a build
// in an environment that strips .git, still produces a working console.
function git(args) {
  try {
    return execFileSync("git", args, {
      cwd: repoRoot,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
  } catch {
    return "";
  }
}
const version = process.env.RELAYPOOL_VERSION || git(["describe", "--tags", "--always"]) || "dev";
const commit = process.env.RELAYPOOL_COMMIT || git(["rev-parse", "--short", "HEAD"]) || "unknown";
const builtAt = process.env.RELAYPOOL_BUILT_AT || new Date().toISOString().replace(/\.\d+Z$/, "Z");

// Substitution values land INSIDE the surrounding quotes of the placeholder in
// the source (`"__BUILD_VERSION__"`), so they must not add quotes of their own;
// escaping keeps a version string containing a quote from breaking the literal.
function literalBody(value) {
  return String(value).replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

const substitutions = [
  { find: "__BUILD_VERSION__", replace: literalBody(version) },
  { find: "__BUILD_COMMIT__", replace: literalBody(commit) },
  { find: "__BUILD_AT__", replace: literalBody(builtAt) },
];

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) out.push(...walk(full));
    else out.push(full);
  }
  return out;
}

// dist/ is fully derived, so it starts clean: a file deleted from src/ must not
// survive in dist/ and keep being served from the embedded copy.
rmSync(outDir, { recursive: true, force: true });
mkdirSync(outDir, { recursive: true });

if (!existsSync(vendorDir)) {
  console.error(`web: vendor directory not found: ${vendorDir}`);
  process.exit(1);
}
for (const file of walk(vendorDir)) {
  const target = join(outDir, "vendor", file.slice(vendorDir.length + 1));
  mkdirSync(dirname(target), { recursive: true });
  cpSync(file, target);
}

let count = 0;
for (const file of walk(srcDir)) {
  const dest = join(outDir, file.slice(srcDir.length + 1));
  mkdirSync(dirname(dest), { recursive: true });
  // 图片按二进制复制，避免 UTF-8 文本替换损坏生成素材。
  if (/\.(?:html|css|js|json|svg|txt)$/i.test(file)) {
    let text = readFileSync(file, "utf8");
    for (const { find, replace } of substitutions) text = text.split(find).join(replace);
    writeFileSync(dest, text);
  } else cpSync(file, dest);
  count++;
}

console.log(`web: built ${count} file(s) -> web/dist (version=${version} commit=${commit})`);
console.log("web: vendored libs:", readdirSync(vendorDir).join(", "));

// 构建与嵌入作为同一步完成，逐文件按字节写入并核验，避免旧控制台被编进新程序。
rmSync(embedDir,{recursive:true,force:true});
for(const file of walk(outDir)) {
	const target=join(embedDir,file.slice(outDir.length+1));
	mkdirSync(dirname(target),{recursive:true});
	const raw=readFileSync(file);writeFileSync(target,raw);
	if(!readFileSync(target).equals(raw))throw new Error("嵌入文件核验失败："+file);
}
console.log("web: 控制台嵌入副本已核验");
