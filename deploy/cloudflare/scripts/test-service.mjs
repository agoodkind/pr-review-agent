import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { cp, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const directory = fileURLToPath(new URL("..", import.meta.url));
const project = "pragent-service-test-" + randomUUID();
const evidence = await mkdtemp(path.join(os.tmpdir(), project + "-"));
const input = path.join(evidence, "input");
await mkdir(input, { mode: 0o700 });
const environment = { ...process.env, PRAGENT_COMPOSE_INPUT: input, PRAGENT_COMPOSE_EVIDENCE: evidence,
  PRAGENT_SERVICE_TEST_REVISION: process.env.PRAGENT_SERVICE_TEST_REVISION ?? process.env.GITHUB_SHA ?? "" };
const module = await readFile(path.resolve(directory, "../../go.mod"), "utf8");
const goVersion = module.match(/^go ([0-9.]+)$/m)?.[1];
if (!goVersion) throw new Error("go.mod must declare a Go version");
environment.PRAGENT_COMPOSE_GO_VERSION = goVersion;
const compose = ["compose", "--project-name", project, "--file", path.join(directory, "test/compose.yaml")];
let child;
let interrupted;
let interruptTimer;
function interrupt(signal) {
  interrupted = signal;
  child?.kill(signal);
  interruptTimer = setTimeout(function () { child?.kill("SIGKILL"); }, 5000);
  interruptTimer.unref();
}
const onInterrupt = function () { interrupt("SIGINT"); };
const onTerminate = function () { interrupt("SIGTERM"); };
process.on("SIGINT", onInterrupt);
process.on("SIGTERM", onTerminate);

async function run(arguments_, cleanup = false, receiveOutput) {
  if (interrupted && !cleanup) throw new Error(`harness interrupted by ${interrupted}`);
  const running = spawn("docker", arguments_, { cwd: directory, env: environment,
    stdio: receiveOutput ? ["ignore", "pipe", "inherit"] : "inherit" });
  if (receiveOutput) running.stdout.on("data", receiveOutput);
  if (!cleanup) child = running;
  return new Promise(function (resolve, reject) {
    running.once("error", reject);
    running.once("close", function (code, signal) {
      if (child === running) child = undefined;
      clearTimeout(interruptTimer);
      resolve(code ?? (signal === "SIGINT" ? 130 : 143));
    });
  });
}
async function checked(arguments_, receiveOutput) {
  const code = await run(arguments_, false, receiveOutput);
  if (code !== 0) {
    process.exitCode = code;
    throw new Error(`docker command failed with exit code ${code}`);
  }
}

process.stdout.write(`Compose project: ${project}\nPrivate integration evidence: ${evidence}\n`);
try {
  const custom = process.argv.slice(2).filter(function (argument, index) { return index !== 0 || argument !== "--"; });
  const binary = process.env.PRAGENT_SERVICE_TEST_BINARY;
  const image = process.env.PRAGENT_SERVICE_TEST_IMAGE;
  if (!custom.length && Boolean(binary) === Boolean(image)) {
    throw new Error("set exactly one of PRAGENT_SERVICE_TEST_BINARY or PRAGENT_SERVICE_TEST_IMAGE");
  }
  if (binary) {
    await cp(path.resolve(binary), path.join(input, "pr-review-agent"));
    environment.PRAGENT_COMPOSE_BINARY = "/input/pr-review-agent";
  }
  if (image) {
    let output = "";
    await checked(["image", "inspect", "--format", "{{.Id}}", image], function (chunk) { output += chunk; });
    const imageID = output.trim();
    if (!/^sha256:[a-f0-9]{64}$/.test(imageID)) throw new Error("Docker did not return the source image ID");
    environment.PRAGENT_COMPOSE_IMAGE_ID = imageID;
    await checked(["save", "--output", path.join(input, "image.tar"), imageID]);
  }
  await checked([...compose, "build", "runner"]);
  await checked([...compose, "up", "--detach", "--wait", "engine"]);
  process.exitCode = await run([...compose, "run", "--rm", "runner", ...custom]);
} catch (error) {
  process.stderr.write(`${error.message}\n`);
  process.exitCode ||= 1;
} finally {
  try {
    const code = await run([...compose, "down", "--volumes", "--remove-orphans", "--rmi", "local", "--timeout", "10"], true);
    if (code !== 0) {
      process.stderr.write(`Compose cleanup failed for ${project} with exit code ${code}\n`);
      process.exitCode ||= code;
    }
  } catch (error) {
    process.stderr.write(`Compose cleanup failed for ${project}: ${error.message}\n`);
    process.exitCode ||= 1;
  }
  process.off("SIGINT", onInterrupt);
  process.off("SIGTERM", onTerminate);
  try {
    await rm(input, { recursive: true });
  } catch (error) {
    process.stderr.write(`Harness input cleanup failed for ${project}: ${error.message}\n`);
    process.exitCode ||= 1;
  }
  if (interrupted) process.exitCode = interrupted === "SIGINT" ? 130 : 143;
}
