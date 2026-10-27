import { query } from "@anthropic-ai/claude-agent-sdk";

export interface ModelEntry {
  value: string;
  displayName: string;
  description: string;
}

type QueryFn = typeof query;

export async function listModels(q: QueryFn = query): Promise<ModelEntry[]> {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  // Streaming-input mode with a prompt that never yields: the SDK initialises and answers supportedModels() without any model turn.
  const silent = (async function* () {
    await gate;
  })();
  const handle = q({
    prompt: silent as never,
    options: { tools: [], settingSources: [], persistSession: false },
  });
  try {
    const models = await handle.supportedModels();
    return models.map((m) => ({ value: m.value, displayName: m.displayName, description: m.description }));
  } finally {
    release();
    handle.close();
  }
}
