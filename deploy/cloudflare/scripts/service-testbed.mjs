import { createHash, generateKeyPairSync, randomUUID } from "node:crypto";
import { cp, readFile, symlink, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const sourceDirectory = fileURLToPath(new URL("..", import.meta.url));
const repository = path.resolve(sourceDirectory, "../..");

export async function prepareServiceTestbed({ directory, image, binary, account, gatewayName, serviceName, gatewayOrigin }) {
  if (!directory || !account || !gatewayName || !serviceName || Boolean(image) === Boolean(binary)) {
    throw new Error("testbed requires a directory, account, gateway and service names, and exactly one image or binary");
  }
  const runtime = JSON.parse(await readFile(path.join(repository, "runtime.json"), "utf8"));
  const recovery = JSON.parse(await readFile(path.join(sourceDirectory, "recovery-test.json"), "utf8"));
  const origin = gatewayOrigin ?? "https://gateway.invalid";
  delete runtime.LOG_FORWARD_URL;
  runtime.PROVIDER_BUDGET_URL = origin + "/internal/v1/provider_budget";
  runtime.REASSESSMENT.queue_url = origin + "/internal/v1/reassessments";
  runtime.REASSESSMENT.enabled = false;
  for (const provider of runtime.PROVIDERS) {
    provider.base_url = "https://provider.invalid";
  }
  await writeFile(path.join(directory, "runtime.json"), JSON.stringify(runtime));
  await cp(path.join(repository, "config"), path.join(directory, "config"), { recursive: true });
  if (image) {
    if (!/^[A-Za-z0-9./_:@-]+$/.test(image)) {
      throw new Error("testbed image reference is invalid");
    }
  } else {
    await cp(binary, path.join(directory, "pr-review-agent"));
  }
  await cp(path.join(sourceDirectory, "test", image ? "service-image.Dockerfile" : "service-binary.Dockerfile"), path.join(directory, "Dockerfile"));
  const gateway = JSON.parse(await readFile(path.join(sourceDirectory, "wrangler.jsonc"), "utf8"));
  const service = JSON.parse(await readFile(path.join(sourceDirectory, "service.wrangler.jsonc"), "utf8"));
  const fixture = path.join(directory, "repository");
  const workers = path.join(fixture, "deploy/cloudflare");
  await cp(path.join(sourceDirectory, "worker"), path.join(workers, "worker"), { recursive: true });
  await cp(path.join(sourceDirectory, "service"), path.join(workers, "service"), { recursive: true });
  await cp(path.join(sourceDirectory, "recovery-test.json"), path.join(workers, "recovery-test.json"));
  await writeFile(path.join(fixture, "runtime.json"), JSON.stringify(runtime));
  await symlink(path.join(sourceDirectory, "node_modules"), path.join(workers, "node_modules"), "dir");
  gateway.name = gatewayName;
  gateway.main = path.join(workers, "worker/index.js");
  gateway.triggers = { crons: [] };
  gateway.vars = { REVIEW_SERVICE_MODE: "legacy" };
  gateway.services = [];
  gateway.account_id = account;
  service.name = serviceName;
  service.main = path.join(workers, "service/index.js");
  service.account_id = account;
  const deploymentToken = randomUUID();
  service.vars = { DEPLOYMENT_TOKEN_SHA256: createHash("sha256").update(deploymentToken).digest("hex") };
  for (const configuration of [gateway, service]) {
    configuration.dev = { inspector_port: 0 };
    configuration.containers[0].image = path.join(directory, "Dockerfile");
    if (image) {
      configuration.containers[0].image_vars = { PR_REVIEW_AGENT_IMAGE: image };
    } else {
      delete configuration.containers[0].image_vars;
    }
  }
  service.services[0].service = gateway.name;
  const key = generateKeyPairSync("rsa", { modulusLength: 2048,
    privateKeyEncoding: { type: "pkcs1", format: "pem" }, publicKeyEncoding: { type: "spki", format: "pem" } });
  const signingMaterial = randomUUID();
  gateway.vars.GITHUB_PRIVATE_KEY = key.privateKey; // gitleaks:allow
  gateway.vars.GITHUB_WEBHOOK_SECRET = signingMaterial; // gitleaks:allow
  for (const provider of runtime.PROVIDERS) {
    gateway.vars[provider.api_key_binding] = randomUUID();
    if (provider.cf_access_client_id_binding) {
      gateway.vars[provider.cf_access_client_id_binding] = randomUUID();
      gateway.vars[provider.cf_access_client_secret_binding] = randomUUID();
    }
  }
  const gatewayPath = path.join(directory, "gateway.json");
  const servicePath = path.join(directory, "service.json");
  await writeFile(gatewayPath, JSON.stringify(gateway), { mode: 0o600 });
  await writeFile(servicePath, JSON.stringify(service), { mode: 0o600 });
  const credentials = Object.fromEntries(Object.entries(gateway.vars).filter(function ([name]) { // gitleaks:allow
    return name !== "REVIEW_SERVICE_MODE";
  }));
  const redactions = [deploymentToken, ...Object.values(credentials)];
  return { gateway, service, gatewayPath, servicePath, runtime, recovery, repository: fixture,
    deploymentToken, signingMaterial, credentials, redactions };
}
