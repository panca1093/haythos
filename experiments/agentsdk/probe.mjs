// Live probe, run by hand: node probe.mjs (uses your Claude login or CLAUDE_CODE_OAUTH_TOKEN).
import { query } from "@anthropic-ai/claude-agent-sdk";

const schema = {
  type: "object",
  properties: { text: { type: "string" }, tool_calls: { type: "array", items: { type: "object" } } },
  required: ["text"],
};

for (let i = 0; i < 3; i++) {
  const t0 = Date.now();
  let turns = 0;
  let structured;
  for await (const m of query({
    prompt: "Reply with text 'pong' and no tool calls.",
    options: { tools: [], maxTurns: 1, outputFormat: { type: "json_schema", schema } },
  })) {
    if (m.type === "assistant") turns++;
    if (m.type === "result") structured = m.structured_output ?? m.result;
  }
  console.log(JSON.stringify({ run: i, ms: Date.now() - t0, assistantTurns: turns, structured }));
}
