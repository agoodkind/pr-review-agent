// deploy-with-image.mjs is the only sanctioned way to deploy by hand.
//
// The committed configuration carries a deliberately unbuildable image
// placeholder, because a raw `wrangler deploy` once shipped a fourteen day old
// digest: the release workflow rewrites the digest at deploy time, a manual
// deploy skipped that step, and the stale image crash looped on its next cold
// start while 33 webhook deliveries died. This script requires the digest to
// be named explicitly, writes it the same way the release workflow does, and
// only then deploys.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const image = process.env.PR_REVIEW_AGENT_IMAGE ?? "";
if (image === "") {
  console.error(
    [
      "PR_REVIEW_AGENT_IMAGE is not set.",
      "",
      "Name the exact image digest to deploy, for example:",
      "  PR_REVIEW_AGENT_IMAGE=ghcr.io/agoodkind/pr-review-agent@sha256:<digest> npm run deploy",
      "",
      "The digest of what production runs now is printed by the latest",
      "release workflow's 'Publish container' job, and by:",
      "  gh release view --json body | jq -r .body",
    ].join("\n"),
  );
  process.exit(1);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const deployDirectory = path.join(scriptDirectory, "..");
const configPath = path.join(deployDirectory, "wrangler.jsonc");

// The committed configuration is never mutated. The digest is written into a
// temporary copy beside it, because relative paths inside the configuration
// resolve from its directory, and the deploy reads that copy. A kill at any
// moment leaves the committed placeholder in place, so a later raw
// `wrangler deploy` still fails at build instead of reusing a stale digest,
// which is the exact outage this script exists to prevent.
const deployConfigPath = path.join(deployDirectory, `wrangler.deploy.${process.pid}.jsonc`);
fs.copyFileSync(configPath, deployConfigPath);
try {
  execFileSync("node", [path.join(scriptDirectory, "configure-image.mjs"), deployConfigPath], {
    cwd: deployDirectory,
    env: process.env,
    stdio: "inherit",
  });
  execFileSync("npm", ["exec", "wrangler", "deploy", "--", "--config", deployConfigPath], {
    cwd: deployDirectory,
    env: process.env,
    stdio: "inherit",
  });
} finally {
  fs.rmSync(deployConfigPath, { force: true });
}
