---
name: verify
description: Verify changes to the saige repo at the public package boundary with a real provider. Use only when working inside the saige repository; never for other projects.
---

# Verifying SAIGE changes

SAIGE is a library: the surface is the public API (`github.com/urmzd/saige/agent`,
`.../agent/tree`, `.../agent/types`, `.../agent/provider/*`). Verify by writing a
small consumer program in the scratchpad, not by running tests.

## Scope

This skill lives in the saige repo and applies only there. Use cloud providers only. Never start, load or call a local Ollama server (`localhost:11434`): it pins a 12+ GB model in RAM and forces the machine into swap.

## Recipe

1. Scratchpad module with a replace directive:

   ```
   module verify
   go 1.25.11
   require github.com/urmzd/saige v0.0.0
   replace github.com/urmzd/saige => <path to this repo's root>
   ```

   Then `go mod tidy && go run .` (tidy pulls deps from the parent go.sum cache).

2. Real providers available via env (verified 2026-07-11, tool-calling
   smoke-tested on all four working keys):
   - `OPENAI_API_KEY`: `openai.NewAdapter(key, "gpt-4o-mini")`, cheap and fast.
   - `GROQ_API_KEY`: `openai.NewAdapter(key, "llama-3.1-8b-instant",
     openai.WithBaseURL("https://api.groq.com/openai/v1"))`, fastest of the
     set (~0.4s/turn).
   - `ANTHROPIC_API_KEY`: `anthropic.NewAdapter(key, "claude-sonnet-5")`.
   - `OLLAMA_API_KEY`: ollama.com cloud (`gpt-oss:20b`); the ollama client
     has no auth support, so inject `Authorization: Bearer` via a custom
     `http.RoundTripper` on `Client.HTTP.Transport`.
   - `GOOGLE_API_KEY`: valid key, but the project's prepaid credits are
     depleted; every call fails with a 429 from `stream.Wait()` at request
     time (not mid-stream). Useful only for testing the request-time error
     path.

3. Driving the agent loop: `agentsdk.NewAgent(AgentConfig{...})`, then
   `stream := ag.Invoke(ctx, []types.Message{types.NewUserMessage(...)})`;
   drain `stream.Deltas()` (collect `types.TextContentDelta`) and check
   `stream.Wait()`.

4. Inspecting conversation state: `tree.FlattenBranchAnnotated(branch)` gives
   role + node state + text per message; `tr.Active()` tells you if compaction
   forked a new branch mid-run.

## Gotchas

- `provider.ChatStream` errors can arrive mid-stream as `types.ErrorDelta`,
  not as the returned error: check both.
- With `CompactSummarize` at small thresholds, compaction fires on almost
  every turn (each one is an extra LLM call); expect multiple `compact*`
  branches in a short conversation.
