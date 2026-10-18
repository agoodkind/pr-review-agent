import { Container } from "@cloudflare/containers";
import { containerLifecycleEvent, containerStoppedEvent } from "./lifecycle.js";
import { signServiceBody } from "./signature.js";

export class ReviewContainer extends Container {
  configureRuntime(configuration) {
    if (!configuration || !Number.isInteger(configuration.port) || configuration.port < 1 ||
        configuration.port > 65535 || typeof configuration.sleepAfter !== "string" ||
        configuration.sleepAfter === "" || !configuration.environment ||
        typeof configuration.environment !== "object" || Array.isArray(configuration.environment) ||
        Object.values(configuration.environment).some(function (value) { return typeof value !== "string"; })) {
      throw new Error("review service requires a valid runtime configuration");
    }
    this.envVars = configuration.environment;
    this.defaultPort = configuration.port;
    this.sleepAfter = configuration.sleepAfter;
  }

  async readDrainState(action) {
    if (!this.ctx.container.running) {
      return { container_running: false };
    }
    if (this.env.RUNTIME_CONFIGURATION) {
      this.configureRuntime(await this.env.RUNTIME_CONFIGURATION.getConfiguration());
    }
    const secret = this.envVars.GITHUB_WEBHOOK_SECRET; // gitleaks:allow
    if (typeof secret !== "string" || secret === "" || !this.defaultPort) {
      throw new Error("review service requires runtime configuration before reading drain state");
    }
    const body = JSON.stringify({ action });
    const signature = await signServiceBody(secret, body);
    let response;
    try {
      response = await this.ctx.container.getTcpPort(this.defaultPort).fetch(new Request("http://container/internal/v1/drain", {
        method: "POST", headers: { "Content-Type": "application/json", "X-Pr-Agent-Drain-Signature": signature },
        body, signal: AbortSignal.timeout(5000),
      }));
    } catch {
      if (!this.ctx.container.running) {
        return { container_running: false };
      }
      throw new Error("review service drain state is unavailable");
    }
    if (!response.ok) {
      throw new Error(`review service drain state returned HTTP ${response.status}`);
    }
    const state = await response.json();
    if (typeof state.admission_closed !== "boolean" ||
        [state.queued, state.running, state.outstanding].some(function (value) { return !Number.isSafeInteger(value) || value < 0; }) ||
        state.queued + state.running !== state.outstanding) {
      throw new Error("review service returned invalid drain state");
    }
    return { container_running: true, admission_closed: state.admission_closed,
      queued: state.queued, running: state.running, outstanding: state.outstanding };
  }

  async drain(action) {
    if (action !== "begin" && action !== "status" && action !== "restart") {
      throw new Error("drain action must be begin, status, or restart");
    }
    const result = await this.ctx.blockConcurrencyWhile(async () => {
      try {
        const state = await this.readDrainState(action === "restart" ? "status" : action);
        if (action === "restart" && state.container_running && state.admission_closed && state.outstanding === 0) {
          await this.destroy();
          return { ok: true, state: { container_running: false } };
        }
        return { ok: true, state };
      } catch (error) {
        return { ok: false, error };
      }
    });
    if (!result.ok) {
      throw result.error;
    }
    return result.state;
  }

  async interruptForRecoveryTest() {
    await this.destroy();
  }

  onStart() {
    console.log(containerLifecycleEvent("container started"));
  }

  onStop(params) {
    console.log(containerStoppedEvent(params));
  }

  async onActivityExpired() {
    await this.ctx.blockConcurrencyWhile(async () => {
      try {
        const state = await this.readDrainState("idle");
        if (!state.container_running) {
          return;
        }
        if (!state.admission_closed || state.outstanding > 0) {
          this.renewActivityTimeout();
          return;
        }
        console.log(containerLifecycleEvent("container idle timer expired", { sleepAfter: this.sleepAfter }));
        await super.onActivityExpired();
      } catch {
        this.renewActivityTimeout();
        console.error(containerLifecycleEvent("container idle check failed; preserving running service"));
      }
    });
  }

  onError(error) {
    console.error(containerLifecycleEvent("container error", { error: String(error) }));
    return error;
  }
}
