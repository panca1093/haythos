// Negative test: run with an invalid CLAUDE_CODE_OAUTH_TOKEN; an auth error means the env token wins over any local login.
import { query } from "@anthropic-ai/claude-agent-sdk";

const redact = (s) => String(s).replace(/sk-ant-[A-Za-z0-9_-]+/g, "[redacted]").slice(0, 300);

try {
  for await (const m of query({
    prompt: "Reply with the single word pong.",
    options: { tools: [], maxTurns: 1, settingSources: [], persistSession: false },
  })) {
    if (m.type === "result") {
      console.log(`result subtype=${m.subtype} is_error=${m.is_error}`);
      if (m.is_error) console.log(`message: ${redact(m.result ?? "")}`);
    }
  }
} catch (e) {
  console.log(`thrown: ${redact(e?.message ?? e)}`);
}
