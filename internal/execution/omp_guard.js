// Loaded explicitly, with ambient extensions disabled. Never throw from a
// tool handler: the harness reports extension errors but may continue the tool.
import { spawn } from "node:child_process";

export default function (pi) {
  const deny = { block: true, reason: "Execution guard unavailable; tool denied" };
  async function check(event, ctx) {
    try {
      const raw = JSON.stringify({session_id: ctx.sessionManager.getSessionId(),
        cwd: ctx.cwd, tool_name: event.toolName, tool_input: event.input});
      const result = await new Promise((resolve, reject) => {
        const child = spawn(BEEKEEPER_BINARY, ["--config", BEEKEEPER_CONFIG,
          "agents", "execution-guard", "--format", "omp"], {stdio: ["pipe", "pipe", "ignore"]});
        let output = "";
        const timer = setTimeout(() => { child.kill(); reject(new Error("guard timeout")); }, 30000);
        child.on("error", reject);
        child.stdout.on("data", data => { output += data; if (output.length > 1048576) child.kill(); });
        child.on("close", code => {
          clearTimeout(timer);
          try { code === 0 ? resolve(JSON.parse(output)) : reject(new Error("guard failed")); }
          catch (error) { reject(error); }
        });
        child.stdin.on("error", reject);
        child.stdin.end(raw);
      });
      if (result.allow === true) return;
      return {block: true, guardVerified: result.allow === false, reason: result.reason || deny.reason};
    } catch { return deny; }
  }
  pi.on("tool_call", check);
  pi.on("session_start", async (_event, ctx) => {
    pi.setActiveTools(["bash", "read", "write"]);
    // Prove the loaded guard fails closed before the runner sends a prompt.
    const result = await check({toolName: "beekeeper_unsupported_probe", input: {}}, ctx);
    if (!result?.block || !result.guardVerified) { ctx.shutdown(); return; }
    process.stdout.write(JSON.stringify({type: "beekeeper_guard_ready", session_id: ctx.sessionManager.getSessionId()}) + "\n");
  });
}
