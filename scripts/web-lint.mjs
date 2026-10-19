#!/usr/bin/env node
// Lint the console sources: tag balance plus the structural invariants that
// have actually broken this page before. Run through `make web-lint`, and as
// part of `make web`.
//
// Each invariant exists because it was a real defect:
//   - an unbalanced tag silently moved the toast element outside #app, so
//     petite-vue never rendered it and success messages simply never appeared
//   - a <template> used as a sibling v-if/v-else branch loses its anchor in
//     petite-vue, throwing "Cannot read properties of null (reading
//     'insertBefore')" at render time

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = process.env.RELAYPOOL_REPO_ROOT || join(here, "..");
const indexPath = process.argv[2] || join(repoRoot, "web", "src", "index.html");

const html = readFileSync(indexPath, "utf8");
let problems = 0;
const fail = (msg) => {
  console.log("  FAIL  " + msg);
  problems++;
};
const ok = (msg) => console.log("  ok    " + msg);

// Comments, script and style bodies are stripped first: their contents are not
// markup and would otherwise be parsed as tags.
const cleaned = html
  .replace(/<!--[\s\S]*?-->/g, "")
  .replace(/<script[\s\S]*?<\/script>/gi, "")
  .replace(/<style[\s\S]*?<\/style>/gi, "")
  .replace(/<!DOCTYPE[^>]*>/i, "");

const VOID = new Set([
  "area", "base", "br", "col", "embed", "hr", "img", "input",
  "link", "meta", "param", "source", "track", "wbr",
]);
const TAG_RE = /<\/?([a-zA-Z][a-zA-Z0-9-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>/g;
const lineOf = (index) => cleaned.slice(0, index).split("\n").length;

// ---- 1. tag balance ----
{
  const stack = [];
  TAG_RE.lastIndex = 0;
  let m;
  while ((m = TAG_RE.exec(cleaned)) !== null) {
    const name = m[1].toLowerCase();
    if (VOID.has(name) || /\/\s*$/.test(m[2] || "")) continue;
    if (m[0].startsWith("</")) {
      if (!stack.length) {
        fail(`line ${lineOf(m.index)}: stray close </${name}>`);
        continue;
      }
      const top = stack.pop();
      if (top.name !== name) {
        fail(`line ${lineOf(m.index)}: </${name}> closes <${top.name}> opened at line ${top.line}`);
      }
    } else {
      stack.push({ name, line: lineOf(m.index) });
    }
  }
  for (const item of stack) fail(`unclosed <${item.name}> opened at line ${item.line}`);
}

// ---- 2. the bound containers must live inside #app ----
{
  const appStart = cleaned.indexOf('id="app"');
  if (appStart < 0) {
    fail('#app container not found');
  } else {
    const appOpen = cleaned.indexOf(">", appStart);
    let depth = 0;
    let end = -1;
    TAG_RE.lastIndex = 0;
    let m;
    // Start scanning from the #app opening tag itself.
    TAG_RE.lastIndex = cleaned.lastIndexOf("<div", appStart);
    while ((m = TAG_RE.exec(cleaned)) !== null) {
      const name = m[1].toLowerCase();
      if (VOID.has(name) || /\/\s*$/.test(m[2] || "")) continue;
      if (m[0].startsWith("</")) {
        depth--;
        if (depth === 0) {
          end = m.index;
          break;
        }
      } else {
        depth++;
      }
    }
    if (end < 0) {
      fail("#app container never closes");
    } else {
      const appBody = cleaned.slice(appOpen, end);
      for (const needle of [["toast.open", "toast"], ["modal.open", "global modal"]]) {
        if (appBody.includes(needle[0])) ok(`${needle[1]} is bound inside #app`);
        else fail(`${needle[1]} (${needle[0]}) is OUTSIDE #app and will never render`);
      }
    }
  }
}

// ---- 3. template anchor hazards ----
//
// petite-vue treats <template v-for> as a fragment. When one of its children has
// a v-if that flips -- or when the fragment is the branch of a v-if/v-else chain
// -- the fragment's insertion anchor can be null, and the framework throws
// "Cannot read properties of null (reading 'insertBefore')". That throw aborts
// the patch midway, so the page is left half-updated and appears frozen.
//
// The observed live failure (2026-10-01) was the request-log table: a
// <template v-for> wrapping a <tr> plus a conditional <tr>. Navigating to
// #/logs threw exactly once, every time.
{
  // 3a. <template> carrying a v-if/v-else/else-if.
  const hazard = /<template[^>]*\sv-(?:if|else|else-if)\b/;
  if (hazard.test(cleaned)) {
    const idx = cleaned.search(hazard);
    fail(`line ${lineOf(idx)}: <template v-if|v-else> is a petite-vue anchor hazard; use a real element`);
  } else {
    ok("no <template v-if|v-else> anchor hazards");
  }

  // 3b. <template v-for> whose body contains a conditional child element.
  //
  // A v-for fragment is safe only when every iteration produces an
  // unconditional set of nodes. A conditional child is what loses the anchor.
  const templateForRe = /<template[^>]*\sv-for\s*=/g;
  let m;
  let conditionalInFor = 0;
  while ((m = templateForRe.exec(cleaned)) !== null) {
    // Find the matching </template> by walking nested <template> tags.
    let depth = 1;
    let cursor = m.index + m[0].length;
    const inner = /<\/?template\b[^>]*>/g;
    inner.lastIndex = cursor;
    let t;
    let end = -1;
    while ((t = inner.exec(cleaned)) !== null) {
      if (t[0].startsWith("</")) {
        depth--;
        if (depth === 0) {
          end = t.index;
          break;
        }
      } else {
        depth++;
      }
    }
    if (end < 0) continue; // unbalanced; the tag check already reported it
    const body = cleaned.slice(cursor, end);
    // Strip any nested <template> bodies so only this level is inspected.
    const own = body.replace(/<template[\s\S]*?<\/template>/g, "");

    // Only a TOP-LEVEL conditional child is dangerous: the fragment's anchor is
    // the position where the next sibling would be inserted, so a conditional
    // that adds or removes a *sibling element* is what can null it. A v-if/A
    // chain nested inside a <td> (a status badge, say) is safe, because the cell
    // itself is unconditional and the anchor lives outside it.
    //
    // Depth is tracked over element tags to find direct children of the
    // fragment.
    const TAG = /<(\/?)([a-zA-Z][a-zA-Z0-9-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>/g;
    let childDepth = 0;
    let t2;
    let topLevelConditional = false;
    TAG.lastIndex = 0;
    while ((t2 = TAG.exec(own)) !== null) {
      const closing = t2[1] === "/";
      const name = t2[2].toLowerCase();
      if (VOID.has(name) || /\/\s*$/.test(t2[3] || "")) {
        // A void element still counts as a direct child when at depth 0.
        if (!closing && childDepth === 0 && /\sv-(?:if|else-if|else)\b/.test(t2[3] || "")) {
          topLevelConditional = true;
        }
        continue;
      }
      if (closing) {
        childDepth--;
        continue;
      }
      if (childDepth === 0 && /\sv-(?:if|else-if|else)\b/.test(t2[3] || "")) {
        topLevelConditional = true;
      }
      childDepth++;
    }

    if (topLevelConditional) {
      conditionalInFor++;
      fail(`line ${lineOf(m.index)}: <template v-for> has a conditional DIRECT child, ` +
        `which can null petite-vue's insert anchor; loop over a real element instead`);
    }
  }
  if (conditionalInFor === 0) {
    ok("no <template v-for> with a conditional child");
  }
}

// ---- 4. build macros still present in source ----
if (cleaned.includes("__BUILD_")) ok("build macros present (substituted by build-web.mjs)");

console.log(problems === 0 ? "\nweb-lint: OK" : `\nweb-lint: ${problems} problem(s)`);
process.exit(problems === 0 ? 0 : 1);
