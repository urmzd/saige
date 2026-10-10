// Package batch runs many independent model calls as one asynchronous job
// behind types.BatchProvider.
//
// Vendor batch APIs (Anthropic Message Batches, the OpenAI Batch API, Gemini
// batch mode and Vertex AI batch prediction) charge about half the
// interactive price and finish within hours. The adapters implement
// types.BatchProvider; Local runs the same requests through any provider with
// bounded concurrency, for runtimes such as Ollama that have no batch API.
//
// A Runner adds what a long-running job needs on top of a BatchProvider:
//
//   - durability: the job record (vendor batch ID, request manifest hash,
//     state) is saved in a Store before anything is submitted, so a crash
//     leaves neither an orphaned nor a duplicate batch. A restarted process
//     calls Submit again with the same job ID and requests and resumes the
//     existing batch. A crash in the middle of a submit is resolved by
//     looking the batch up where the vendor allows (types.BatchFinder), and
//     otherwise stops with ErrIndeterminate until Reconcile decides (D-14).
//   - completion: Wait polls with backoff and wakes early on a Notifier
//     message, which Sweep publishes when it sees a job end.
//   - cost: the budget is reserved per request at submit, at the batch rate
//     card (types.Pricing.Batch), and settled per result. Requests that
//     failed, were canceled or expired are not billed and release their
//     reservation.
//
// Coalescer turns concurrent single calls into batches, so code written
// against types.Provider, such as eval subjects and judges, can run its calls
// as one batch without changing shape.
//
// Multi-turn agent loops are not batched: each turn depends on the previous
// turn's tool results, so a loop would wait hours per turn. Batch single-turn
// work (agent.RunBatch, AIFunction.Batch, eval subjects and judges).
package batch
