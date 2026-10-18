import { spawn, execFile } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import fs from "node:fs/promises";
import { openSync, closeSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { promisify, parseArgs } from "node:util";
import { fileURLToPath } from "node:url";
import { prepareServiceTestbed } from "./service-testbed.mjs";

const execute = promisify(execFile);
const sourceDirectory = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repository = path.resolve(sourceDirectory, "../..");
const workerChanges = await execute("git", ["status", "--porcelain", "--", "deploy/cloudflare/worker", "deploy/cloudflare/service"], { cwd: repository });
if (workerChanges.stdout.trim() !== "") throw new Error("Commit Worker source changes before running the isolated deployment proof");
const { values } = parseArgs({ options: {
  "account-token-file": { type: "string" },
  "baseline-run": { type: "string" }, "baseline-revision": { type: "string" },
  "baseline-worker-revision": { type: "string" },
  "candidate-run": { type: "string" }, "candidate-revision": { type: "string" },
  "output-dir": { type: "string" },
  "candidate-timeout": { type: "string", default: "6m" },
} });
for (const name of ["account-token-file", "baseline-run", "baseline-revision", "baseline-worker-revision", "candidate-run", "candidate-revision"]) {
  if (!values[name]) throw new Error(`Missing --${name}`);
}
for (const name of ["baseline-revision", "baseline-worker-revision", "candidate-revision"]) {
  if (!/^[a-f0-9]{40}$/.test(values[name])) throw new Error(`Invalid --${name}`);
}
for (const name of ["baseline-run", "candidate-run"]) {
  if (!/^[0-9]+$/.test(values[name])) throw new Error(`Invalid --${name}`);
}
const directory = values["output-dir"] ?? await fs.mkdtemp(path.join(os.tmpdir(), "pragent-deployment-test-"));
if (values["output-dir"]) await fs.mkdir(directory, { mode: 0o700 });
await fs.chmod(directory, 0o700);
const prefix = "pragent-deploy-test-" + randomBytes(4).toString("hex");
const gatewayName = prefix + "-gateway";
const slotNames = [prefix + "-service-a", prefix + "-service-b"];
const sourceGateway = JSON.parse(await fs.readFile(path.join(sourceDirectory, "wrangler.jsonc"), "utf8"));
const sourceDeployment = JSON.parse(await fs.readFile(path.join(sourceDirectory, "deployment.json"), "utf8"));
const sourcePackage = JSON.parse(await fs.readFile(path.join(sourceDirectory, "package.json"), "utf8"));
const priorPackage = JSON.parse((await execute("git", ["show", values["baseline-worker-revision"] + ":deploy/cloudflare/package.json"], { cwd: repository })).stdout);
if (sourcePackage.dependencies["@cloudflare/containers"] !== priorPackage.dependencies["@cloudflare/containers"]) {
  throw new Error("The prior Worker requires a different Container SDK version");
}
const hostname = new URL(sourceDeployment.gateway_origin).hostname;
const workersDomain = hostname.slice(hostname.indexOf(".") + 1);
if (!workersDomain.endsWith(".workers.dev")) throw new Error("Expected a configured workers.dev subdomain");
const gatewayOrigin = `https://${gatewayName}.${workersDomain}`;
const sourceCommit = (await execute("git", ["rev-parse", "HEAD"], { cwd: repository })).stdout.trim();
await writeJSON(path.join(directory, "resources.json"), { prefix, account: sourceGateway.account_id,
  gateway: gatewayName, slots: slotNames, gateway_origin: gatewayOrigin,
  baseline_worker_revision: values["baseline-worker-revision"], candidate_worker_revision: sourceCommit,
  container_sdk: sourcePackage.dependencies["@cloudflare/containers"] });

async function writeJSON(filename, value) {
  await fs.writeFile(filename, JSON.stringify(value, null, 2) + "\n", { mode: 0o600 });
}

async function run(stage, command, args, { cwd = repository, input } = {}) {
  const log = path.join(directory, stage + ".log");
  const descriptor = openSync(log, "a", 0o600);
  try {
    await new Promise(function (resolve, reject) {
      const child = spawn(command, args, { cwd, stdio: [input === undefined ? "ignore" : "pipe", descriptor, descriptor] });
      child.on("error", reject);
      child.on("exit", function (code, signal) {
        if (code === 0) resolve();
        else reject(new Error(`${stage} failed with ${signal ?? code}; private evidence: ${log}`));
      });
      if (input !== undefined) child.stdin.end(input);
    });
  } finally {
    closeSync(descriptor);
  }
}

async function canonicalRelease(label, runID, revision) {
  const release = path.join(directory, label + "-release");
  await fs.mkdir(release, { mode: 0o700 });
  await run(label + "-download", "gh", ["run", "download", runID, "--repo", "agoodkind/pr-review-agent", "--name", "release-linux-amd64", "--dir", release]);
  const archive = path.join(release, "pr-review-agent_linux_amd64.tar.gz");
  const verification = await execute("gh", ["attestation", "verify", archive, "--repo", "agoodkind/pr-review-agent",
    "--signer-workflow", "agoodkind/go-makefile/.github/workflows/_package.yml", "--source-digest", revision, "--format", "json"], { maxBuffer: 8 * 1024 * 1024 });
  await fs.writeFile(path.join(release, "attestation.json"), verification.stdout, { mode: 0o600 });
  await run(label + "-extract", "tar", ["-xzf", archive, "-C", release, "pr-review-agent"]);
  return { archive, binary: path.join(release, "pr-review-agent"), revision, run: runID,
    archive_sha256: createHash("sha256").update(await fs.readFile(archive)).digest("hex") };
}

const baseline = await canonicalRelease("baseline", values["baseline-run"], values["baseline-revision"]);
const candidate = await canonicalRelease("candidate", values["candidate-run"], values["candidate-revision"]);
const fixtureDirectory = path.join(directory, "fixture");
await fs.mkdir(fixtureDirectory, { mode: 0o700 });
const fixture = await prepareServiceTestbed({ directory: fixtureDirectory, binary: baseline.binary,
  account: sourceGateway.account_id, gatewayName, serviceName: slotNames[0], gatewayOrigin });
if (fixture.runtime.LOG_FORWARD_URL !== undefined || fixture.runtime.REASSESSMENT.enabled !== false ||
    fixture.runtime.PROVIDER_BUDGET_URL !== gatewayOrigin + "/internal/v1/provider_budget" ||
    fixture.runtime.REASSESSMENT.queue_url !== gatewayOrigin + "/internal/v1/reassessments" ||
    !fixture.runtime.PROVIDERS.every(function (provider) { return new URL(provider.base_url).hostname.endsWith(".invalid"); })) {
  throw new Error("Test runtime does not isolate external callbacks and providers");
}

const authentication = await execute("gh", ["auth", "token"], { maxBuffer: 1024 * 1024 });
await run("registry-login", "docker", ["login", "ghcr.io", "--username", "agoodkind", "--password-stdin"], { input: authentication.stdout });

async function publishImage(label, release) {
  const context = path.join(directory, label + "-image");
  await fs.mkdir(path.join(context, "dist", "pr-review-agent_linux_amd64"), { recursive: true, mode: 0o700 });
  const dockerfile = await execute("git", ["show", release.revision + ":Dockerfile"], { cwd: repository });
  await fs.writeFile(path.join(context, "Dockerfile"), dockerfile.stdout);
  await run(label + "-config-source", "git", ["archive", "--format=tar", "--output=" + path.join(context, "source.tar"), release.revision, "config"]);
  await run(label + "-config-extract", "tar", ["-xf", path.join(context, "source.tar"), "-C", context]);
  await fs.copyFile(release.binary, path.join(context, "dist", "pr-review-agent_linux_amd64", "pr-review-agent"));
  await fs.writeFile(path.join(context, "runtime.json"), JSON.stringify(fixture.runtime));
  const tag = "ghcr.io/agoodkind/pr-review-agent:deployment-test-" + prefix.slice(-8) + "-" + release.revision.slice(0, 7);
  await run(label + "-image-build", "docker", ["buildx", "build", "--platform", "linux/amd64", "--load", "--build-arg", "SOURCE_REVISION=" + release.revision,
    "--build-arg", "VERSION=deployment-test-" + release.revision.slice(0, 7), "--tag", tag, context]);
  await run(label + "-image-version", "docker", ["run", "--rm", "--platform", "linux/amd64", tag, "--version"]);
  await run(label + "-image-push", "docker", ["push", tag]);
  const inspection = await execute("docker", ["image", "inspect", tag, "--format", "{{json .RepoDigests}}"]);
  const image = JSON.parse(inspection.stdout).find(function (digest) { return /^ghcr\.io\/agoodkind\/pr-review-agent@sha256:[a-f0-9]{64}$/.test(digest); });
  if (!image) throw new Error("Published test image did not return an immutable registry digest");
  const record = { ...release, image, tag, runtime_sha256: createHash("sha256").update(JSON.stringify(fixture.runtime)).digest("hex"),
    source_runtime_modified: true, container_attestation: false };
  await writeJSON(path.join(directory, label + "-image.json"), record);
  return image;
}

const baselineImage = await publishImage("baseline", baseline);
const candidateImage = await publishImage("candidate", candidate);
const wrapper = path.join(fixtureDirectory, "service.Dockerfile");
await fs.copyFile(path.join(sourceDirectory, "Dockerfile"), wrapper);
fixture.gateway.containers[0].image = wrapper;
fixture.gateway.containers[0].name = gatewayName;
fixture.gateway.containers[0].image_vars = { PR_REVIEW_AGENT_IMAGE: baselineImage };
fixture.service.containers[0].image = wrapper;
fixture.service.containers[0].name = slotNames[0];
fixture.service.containers[0].image_vars = { PR_REVIEW_AGENT_IMAGE: candidateImage };
delete fixture.service.vars.DEPLOYMENT_TOKEN_SHA256;
const deploymentDirectory = path.join(fixture.repository, "deploy", "cloudflare");
const gatewayPath = path.join(deploymentDirectory, "gateway.json");
const servicePath = path.join(deploymentDirectory, "service.json");
const deploymentPath = path.join(deploymentDirectory, "deployment.json");
const baselineGatewayPath = path.join(deploymentDirectory, "baseline-gateway.json");
const priorSource = path.join(directory, "prior-source");
await fs.mkdir(priorSource, { mode: 0o700 });
await run("prior-worker-source", "git", ["archive", "--format=tar", "--output=" + path.join(priorSource, "worker.tar"), values["baseline-worker-revision"], "deploy/cloudflare/worker"]);
await run("prior-worker-extract", "tar", ["-xf", path.join(priorSource, "worker.tar"), "-C", priorSource]);
await fs.cp(path.join(priorSource, "deploy/cloudflare/worker"), path.join(deploymentDirectory, "prior-worker"), { recursive: true });
const baselineGateway = structuredClone(fixture.gateway);
baselineGateway.main = path.join(deploymentDirectory, "prior-worker/index.js");
await writeJSON(baselineGatewayPath, baselineGateway);
await writeJSON(gatewayPath, fixture.gateway);
await writeJSON(servicePath, fixture.service);
await writeJSON(deploymentPath, { gateway_origin: gatewayOrigin,
  slots: slotNames.map(function (name) { return { name, origin: `https://${name}.${workersDomain}` }; }),
  candidate_timeout: values["candidate-timeout"], probe_interval: "2s" });
await run("cloudflare-test", "go", ["run", "./cmd/pragent-ops", "deployment-test", "--wrangler", gatewayPath,
  "--baseline-wrangler", baselineGatewayPath,
  "--service-wrangler", servicePath, "--deployment", deploymentPath, "--baseline-image", baselineImage,
  "--baseline-revision", baseline.revision, "--image", candidateImage, "--revision", candidate.revision,
  "--account-token-file", values["account-token-file"], "--output-dir", path.join(directory, "cloudflare")]);
process.stdout.write(JSON.stringify({ directory, gateway: gatewayName, baseline_image: baselineImage, candidate_image: candidateImage }) + "\n");
