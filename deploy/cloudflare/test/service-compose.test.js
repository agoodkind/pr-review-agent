import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const directory = fileURLToPath(new URL("..", import.meta.url));
const enabled = process.env.PRAGENT_COMPOSE_LIFECYCLE_TEST === "1";

test("Compose removes the isolated daemon and its resources after success, failure, and interruption", { skip: !enabled, timeout: 900000 }, async function (context) {
  for (const mode of ["setup-failure", "success", "image-input", "failure", "SIGINT", "SIGTERM"]) {
    let image = "";
    if (mode === "image-input") {
      image = execFileSync("docker", ["image", "inspect", "--format", "{{index .RepoDigests 0}}", "docker:28-dind"], { encoding: "utf8" }).trim();
      assert.match(image, /@sha256:[a-f0-9]{64}$/);
    }
    const child = spawn(process.execPath, ["scripts/test-service.mjs", "--", "node", "test/service-compose-fixture.mjs", mode], {
      cwd: directory, stdio: ["ignore", "pipe", "pipe"],
      env: { ...process.env, PRAGENT_SERVICE_TEST_BINARY: mode === "setup-failure" ? path.join(directory, randomUUID()) : "", PRAGENT_SERVICE_TEST_IMAGE: image },
    });
    let output = "";
    let signaled = false;
    function receive(data) {
      output += data;
      if (mode.startsWith("SIG") && !signaled && output.includes("lifecycle fixture ready")) {
        signaled = true;
        child.kill(mode);
      }
    }
    child.stdout.on("data", receive);
    child.stderr.on("data", receive);
    const exited = new Promise(function (resolve, reject) {
      child.once("error", reject);
      child.once("exit", resolve);
    });
    context.after(async function () {
      if (child.exitCode === null && child.signalCode === null) {
        child.kill("SIGTERM");
        await exited;
      }
    });
    const code = await exited;
    const expected = { "setup-failure": 1, success: 0, "image-input": 0, failure: 23, SIGINT: 130, SIGTERM: 143 }[mode];
    assert.equal(code, expected, output);
    const project = output.match(/Compose project: (pragent-service-test-[a-f0-9-]+)/)?.[1];
    assert.ok(project, output);
    if (mode !== "setup-failure") assert.match(output, /lifecycle fixture ready/, output);
    for (const [resource, arguments_] of [
      ["containers", ["ps", "--all", "--quiet"]],
      ["volumes", ["volume", "ls", "--quiet"]],
      ["networks", ["network", "ls", "--quiet"]],
      ["images", ["image", "ls", "--quiet"]],
    ]) {
      const remaining = execFileSync("docker", [...arguments_, "--filter", `label=com.docker.compose.project=${project}`], { encoding: "utf8" });
      assert.equal(remaining.trim(), "", `${mode} retained ${resource}: ${remaining}`);
    }
    const images = execFileSync("docker", ["image", "ls", "--quiet", `${project}-runner`], { encoding: "utf8" });
    assert.equal(images.trim(), "", `${mode} retained its runner image`);
    context.diagnostic(`${mode}: ${project} exited ${code}; Docker removed its containers, volumes, network, and image`);
  }
});
