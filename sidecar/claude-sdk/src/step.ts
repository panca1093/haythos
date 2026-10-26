import { query } from "@anthropic-ai/claude-agent-sdk";

export interface StepParams {
  system: string;
  prompt: string;
  schema: Record<string, unknown>;
  tools: string[];
}

export interface StepResult {
  text: string;
  usage: { input_tokens: number; output_tokens: number };
}

type QueryFn = typeof query;

export async function step(p: StepParams, q: QueryFn = query): Promise<StepResult> {
  if (!Array.isArray(p.tools) || p.tools.length !== 0) {
    throw new Error("SDK built-in tools are never enabled");
  }
  let text = "";
  let usage = { input_tokens: 0, output_tokens: 0 };
  // TODO: uncertain — json_schema output may consume an extra SDK turn so maxTurns: 1 could fail live, verify before prod
  const stream = q({
    prompt: p.prompt,
    options: {
      systemPrompt: p.system,
      tools: [],
      maxTurns: 1,
      outputFormat: { type: "json_schema", schema: p.schema },
      settingSources: [],
      persistSession: false,
    },
  });
  for await (const m of stream) {
    if (m.type !== "result") continue;
    if (m.subtype !== "success") throw new Error(`sdk result: ${m.subtype}`);
    text = m.structured_output !== undefined ? JSON.stringify(m.structured_output) : m.result;
    usage = { input_tokens: m.usage.input_tokens ?? 0, output_tokens: m.usage.output_tokens ?? 0 };
  }
  return { text, usage };
}
