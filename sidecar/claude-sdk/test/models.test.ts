import test from "node:test";
import assert from "node:assert/strict";
import { listModels } from "../src/models.ts";

type Opts = { tools?: unknown; settingSources?: unknown; persistSession?: unknown };

function fakeQuery(seen: { opts?: Opts; prompt?: AsyncIterable<unknown>; closed: number; pulled: number }, models: unknown[] | Error) {
  return ((args: { prompt: AsyncIterable<unknown>; options: Opts }) => {
    seen.opts = args.options;
    seen.prompt = args.prompt;
    return {
      supportedModels: async () => {
        const it = args.prompt[Symbol.asyncIterator]();
        void it.next().then(() => seen.pulled++);
        if (models instanceof Error) throw models;
        return models;
      },
      close: () => {
        seen.closed++;
      },
    };
  }) as never;
}

test("lists models through the streaming-input query without sending a turn, with the safe options", async () => {
  const seen = { closed: 0, pulled: 0 } as { opts?: Opts; prompt?: AsyncIterable<unknown>; closed: number; pulled: number };
  const out = await listModels(
    fakeQuery(seen, [
      { value: "default", displayName: "Default", description: "Recommended", supportsEffort: true },
      { value: "haiku", displayName: "Haiku", description: "Fast" },
    ]),
  );
  assert.deepEqual(out, [
    { value: "default", displayName: "Default", description: "Recommended" },
    { value: "haiku", displayName: "Haiku", description: "Fast" },
  ]);
  assert.deepEqual(seen.opts?.tools, []);
  assert.deepEqual(seen.opts?.settingSources, []);
  assert.equal(seen.opts?.persistSession, false);
  assert.equal(typeof seen.prompt?.[Symbol.asyncIterator], "function");
  assert.equal(seen.pulled, 0);
  assert.equal(seen.closed, 1);
});

test("closes the query when listing fails", async () => {
  const seen = { closed: 0, pulled: 0 } as { opts?: Opts; prompt?: AsyncIterable<unknown>; closed: number; pulled: number };
  await assert.rejects(listModels(fakeQuery(seen, new Error("auth failed"))), /auth failed/);
  assert.equal(seen.closed, 1);
});
