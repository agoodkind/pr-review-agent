import { DurableObject } from "cloudflare:workers";
import usageReport from "../worker/budget-report.sql";
import usageDetails from "../worker/budget-details.sql";
import usageSDK from "../worker/budget-sdk.sql";
import deleteMarker from "../worker/budget-totals-delete-marker.sql";

export class MeasuredProviderBudget extends DurableObject {
  async fetch(request) {
    const payload = await request.json();
    const { provider_id: provider, model, admitted_at_ms: admittedAt,
      input_tokens: input, output_tokens: output, total_tokens: total,
      sdk_usage: sdk, bucket_width_ms: width } = payload;
    this.ctx.storage.transactionSync(() => {
      const inserted = this.ctx.storage.sql.exec(usageReport, provider, model, admittedAt,
        input, output, total).one();
      this.ctx.storage.sql.exec(usageDetails, inserted.sequence, null, null, null);
      this.ctx.storage.sql.exec(usageSDK, inserted.sequence, JSON.stringify(sdk));
      this.ctx.storage.sql.exec(deleteMarker, provider, model, width,
        Math.floor(admittedAt / width) * width);
    });
    return Response.json({ reported: true, sdk_usage_stored: true });
  }
}

export default {
  fetch(request, env) {
    return env.PROVIDER_BUDGET.getByName("provider-budgets").fetch(request);
  },
};
