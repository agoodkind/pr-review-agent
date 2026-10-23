import { spawnSync } from "node:child_process";
import assert from "node:assert/strict";

if (process.argv[2] === "image-input") {
  const image = spawnSync("docker", ["image", "inspect", "--format", "{{.Id}}", process.env.PRAGENT_SERVICE_TEST_IMAGE], { encoding: "utf8" });
  assert.equal(image.status, 0, image.stderr);
  assert.equal(image.stdout.trim(), process.env.PRAGENT_COMPOSE_IMAGE_ID);
}

const result = spawnSync("docker", ["run", "--detach", "--name", "intentionally-retained-fixture", "alpine:3.22", "sleep", "3600"], { stdio: "inherit" });
if (result.status !== 0) process.exit(result.status ?? 1);
process.stdout.write("lifecycle fixture ready\n");
if (["success", "image-input"].includes(process.argv[2])) process.exit(0);
if (process.argv[2] === "failure") process.exit(23);
setInterval(function () {}, 1000);
