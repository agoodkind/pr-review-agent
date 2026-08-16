import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const imagePattern = /^ghcr\.io\/agoodkind\/pr-review-agent@sha256:[a-f0-9]{64}$/;

function configureImage(configPath, image) {
  if (!imagePattern.test(image)) {
    throw new Error("PR_REVIEW_AGENT_IMAGE must be an immutable pr-review-agent digest");
  }

  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  const container = config.containers?.find(function findContainer(candidate) {
    return candidate.class_name === "PrAgentContainer";
  });
  if (!container) {
    throw new Error("PrAgentContainer is missing from the Wrangler configuration");
  }

  container.image_vars = { PR_REVIEW_AGENT_IMAGE: image };
  fs.writeFileSync(configPath, `${JSON.stringify(config, null, 2)}\n`);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const configPath = process.argv[2] ?? path.join(scriptDirectory, "..", "wrangler.jsonc");
configureImage(configPath, process.env.PR_REVIEW_AGENT_IMAGE ?? "");
