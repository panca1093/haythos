import { createInterface } from "node:readline";
import { step, type StepParams } from "./step.ts";

type Request = { jsonrpc: "2.0"; id: number; method: string; params?: unknown };

function reply(id: number, body: { result: unknown } | { error: { message: string } }): void {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id, ...body }) + "\n");
}

export async function handle(req: Request): Promise<void> {
  try {
    switch (req.method) {
      case "ping":
        return reply(req.id, { result: "pong" });
      case "step":
        return reply(req.id, { result: await step(req.params as StepParams) });
      default:
        return reply(req.id, { error: { message: `unknown method ${req.method}` } });
    }
  } catch (e) {
    reply(req.id, { error: { message: e instanceof Error ? e.message : String(e) } });
  }
}

const rl = createInterface({ input: process.stdin });
for await (const line of rl) {
  if (!line.trim()) continue;
  await handle(JSON.parse(line) as Request);
}
