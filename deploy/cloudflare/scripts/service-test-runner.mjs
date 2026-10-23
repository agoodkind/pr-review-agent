import { spawn } from "node:child_process";
import { access } from "node:fs/promises";

async function run(command, arguments_) {
  const child = spawn(command, arguments_, { stdio: "inherit" });
  return new Promise(function (resolve, reject) {
    child.once("error", reject);
    child.once("exit", function (code, signal) { resolve(code ?? (signal === "SIGINT" ? 130 : 143)); });
  });
}

if (process.env.PRAGENT_SERVICE_TEST_IMAGE) {
  await access("/input/image.tar");
  const code = await run("docker", ["load", "--input", "/input/image.tar"]);
  if (code !== 0) process.exit(code);
  const imageID = process.env.PRAGENT_COMPOSE_IMAGE_ID;
  if (!/^sha256:[a-f0-9]{64}$/.test(imageID ?? "")) throw new Error("the harness requires the source image ID");
  const localImage = "pragent-test-source:local";
  const tagCode = await run("docker", ["tag", imageID, localImage]);
  if (tagCode !== 0) process.exit(tagCode);
  process.env.PRAGENT_SERVICE_TEST_IMAGE = localImage;
}
process.exitCode = await run("node", ["--test", "test/service-routing.test.js"]);
