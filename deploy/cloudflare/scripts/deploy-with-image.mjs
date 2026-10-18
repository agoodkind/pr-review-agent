import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const image = process.env.PR_REVIEW_AGENT_IMAGE;
const revision = process.env.EXPECTED_REVISION;
const token = process.env.CLOUDFLARE_API_TOKEN;
if (!image || !revision || !token) {
  throw new Error("Set PR_REVIEW_AGENT_IMAGE, EXPECTED_REVISION, and CLOUDFLARE_API_TOKEN before deployment");
}

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
const directory = fs.mkdtempSync(path.join(os.tmpdir(), "pragent-deploy-credential-"));
const tokenPath = path.join(directory, "token");
try {
  fs.writeFileSync(tokenPath, token, { mode: 0o600, flag: "wx" });
  const environment = { ...process.env };
  delete environment.CLOUDFLARE_API_TOKEN;
  execFileSync("go", [
    "run", "./cmd/pragent-ops", "deploy",
    "--token-file", tokenPath,
    "--image", image,
    "--revision", revision,
    ...process.argv.slice(2),
  ], { cwd: root, env: environment, stdio: "inherit" });
} finally {
  fs.rmSync(directory, { recursive: true, force: true });
}
