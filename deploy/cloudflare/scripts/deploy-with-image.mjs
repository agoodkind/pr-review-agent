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

// The configuration is restored whether the deploy succeeds or fails. Leaving
// the real digest written would let a later raw `wrangler deploy` reuse it
// once it goes stale, which is the exact outage this script exists to prevent.
const committedConfig = fs.readFileSync(configPath, "utf8");
try {
  execFileSync("node", [path.join(scriptDirectory, "configure-image.mjs")], {
    cwd: deployDirectory,
    env: process.env,
    stdio: "inherit",
  });
  execFileSync("npm", ["exec", "wrangler", "deploy"], {
    cwd: deployDirectory,
    env: process.env,
    stdio: "inherit",
  });
} finally {
  fs.writeFileSync(configPath, committedConfig);
}
