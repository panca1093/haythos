import test from "node:test";
import assert from "node:assert/strict";
import { step } from "../src/step.ts";

type Opts = { tools?: unknown; maxTurns?: number; settingSources?: unknown; model?: string };

function fakeQuery(seen: { opts?: Opts }, msg: Record<string, unknown>) {
  return ((args: { options: Opts }) => {
    seen.opts = args.options;
    return (async function* () {
      yield msg;
    })();
  }) as never;
}

const params = { system: "s", prompt: "p", schema: { type: "object" }, tools: [] as string[] };

test("calls the SDK with no built-in tools and a single turn", async () => {
  const seen: { opts?: Opts } = {};
  const out = await step(
    params,
    fakeQuery(seen, { type: "result", subtype: "success", result: "raw", structured_output: { text: "hi" }, usage: { input_tokens: 3, output_tokens: 2 } }),
  );
  assert.deepEqual(seen.opts?.tools, []);
  assert.equal(seen.opts?.maxTurns, 1);
  assert.deepEqual(seen.opts?.settingSources, []);
  assert.equal(out.text, '{"text":"hi"}');
  assert.deepEqual(out.usage, { input_tokens: 3, output_tokens: 2 });
});

test("falls back to result text when there is no structured output", async () => {
  const out = await step(params, fakeQuery({}, { type: "result", subtype: "success", result: '{"text":"x"}', usage: {} }));
  assert.equal(out.text, '{"text":"x"}');
});

test("refuses a request that enables built-in tools", async () => {
  await assert.rejects(step({ ...params, tools: ["Bash"] }, fakeQuery({}, {})), /never enabled/);
});

test("surfaces SDK errors", async () => {
  await assert.rejects(step(params, fakeQuery({}, { type: "result", subtype: "error_max_turns" })), /error_max_turns/);
});

test("passes options.model only when a model is given", async () => {
  const ok = { type: "result", subtype: "success", result: "{}", usage: {} };
  const withModel: { opts?: Opts } = {};
  await step({ ...params, model: "claude-sonnet-4-5" }, fakeQuery(withModel, ok));
  assert.equal(withModel.opts?.model, "claude-sonnet-4-5");
  for (const model of [undefined, ""]) {
    const seen: { opts?: Opts } = {};
    await step({ ...params, model }, fakeQuery(seen, ok));
    assert.equal("model" in (seen.opts ?? {}), false);
  }
});
