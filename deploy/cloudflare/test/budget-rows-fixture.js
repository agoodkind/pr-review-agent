import { ProviderBudget } from "../worker/budget-do.js";
import { routeRequest } from "../worker/router.js";
import seedUsage from "./budget-rows-seed.sql";

export class MeasuredProviderBudget extends ProviderBudget {
  constructor(ctx, env) {
    super(ctx, env);
    this.cursors = [];
    const execute = ctx.storage.sql.exec.bind(ctx.storage.sql);
    ctx.storage.sql.exec = (...arguments_) => {
      const cursor = execute(...arguments_);
      this.cursors.push(cursor);
      return cursor;
    };
  }

  async fetch(request) {
    if (new URL(request.url).pathname === "/__test/seed") {
      const { count, provider, model, start } = await request.json();
      this.ctx.storage.sql.exec(seedUsage, count, provider, model, start).toArray();
      this.cursors = [];
      return Response.json({ seeded: count });
    }
    this.cursors = [];
    const response = await super.fetch(request);
    const headers = new Headers(response.headers);
    headers.set("X-Test-SQL-Rows-Read", String(this.cursors.reduce(function (total, cursor) {
      return total + cursor.rowsRead;
    }, 0)));
    headers.set("X-Test-SQL-Rows-Written", String(this.cursors.reduce(function (total, cursor) {
      return total + cursor.rowsWritten;
    }, 0)));
    return new Response(response.body, { status: response.status, headers });
  }
}

export default {
  fetch(request, env, ctx) {
    if (new URL(request.url).pathname === "/__test/seed") {
      return env.PROVIDER_BUDGET.getByName("provider-budgets").fetch(request);
    }
    return routeRequest(request, env, ctx);
  },
};
