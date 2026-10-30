"""Aggregate bench/results/*.jsonl and the collector output into results.md."""

import collections
import json
import statistics
from pathlib import Path

HERE = Path(__file__).resolve().parent
RESULTS = HERE / "results"
ORDER = ["saige", "pydantic-ai", "openai-agents", "claude-agent-sdk"]


def load_runs():
    """Read every round file. Process stats are the median across rounds."""
    runs, proc_recs = [], collections.defaultdict(list)
    for path in sorted((RESULTS / "raw").glob("*.jsonl")):
        for line in path.read_text().splitlines():
            rec = json.loads(line)
            if rec["type"] == "process":
                proc_recs[rec["framework"]].append(rec)
            elif not rec["warmup"]:
                runs.append(rec)
    procs = {fw: {k: statistics.median(r[k] for r in rs) for k in ("init_ms", "rss_mb", "child_rss_mb")}
             for fw, rs in proc_recs.items()}
    return runs, procs


def pct(values, q):
    values = sorted(values)
    if not values:
        return None
    k = (len(values) - 1) * q
    lo, hi = int(k), min(int(k) + 1, len(values) - 1)
    return values[lo] + (values[hi] - values[lo]) * (k - lo)


def fmt(v, unit=""):
    return "n/a" if v is None else f"{v:,.0f}{unit}"


def attrs(item):
    out = {}
    for a in item.get("attributes", []):
        v = a["value"]
        out[a["key"]] = next(iter(v.values())) if v else None
    return out


def load_otel():
    """Return per-service spans, log records and metric names."""
    spans, logs, metrics = collections.defaultdict(list), collections.Counter(), collections.defaultdict(set)
    for kind, key, scope_key, item_key in [
        ("traces", "resourceSpans", "scopeSpans", "spans"),
        ("logs", "resourceLogs", "scopeLogs", "logRecords"),
        ("metrics", "resourceMetrics", "scopeMetrics", "metrics"),
    ]:
        path = RESULTS / "otel" / f"{kind}.jsonl"
        if not path.exists():
            continue
        # Truncating a file the collector holds open leaves null padding.
        for line in path.read_text().replace("\0", "").splitlines():
            if not line.strip():
                continue
            for res in json.loads(line).get(key, []):
                svc = attrs(res["resource"]).get("service.name", "?")
                for scope in res.get(scope_key, []):
                    for item in scope.get(item_key, []):
                        if kind == "traces":
                            spans[svc].append(item)
                        elif kind == "logs":
                            logs[svc] += 1
                        else:
                            metrics[svc].add(item["name"])
    return spans, logs, metrics


LLM_SPAN = ("chat ", "Chat completion", "claude_code.llm_request")
TOOL_SPAN = ("execute_tool", "Function:", "claude_code.tool")


def observability(fw, all_runs, spans, logs, metrics):
    """Each signal is checked on the span type it belongs to."""
    sp = spans.get(fw, [])
    n_runs = all_runs.get(fw, 1)
    llm = [attrs(s) for s in sp if s["name"].startswith(LLM_SPAN)]
    tool = [(s, attrs(s)) for s in sp if s["name"].startswith(TOOL_SPAN)]
    every = [attrs(s) for s in sp]
    yes = lambda b: "yes" if b else "no"  # noqa: E731
    some = lambda rows, keys: any(any(k in a for k in keys) for a in rows)  # noqa: E731
    prompt = some(every, ["gen_ai.input.messages", "input", "user_prompt"])
    response = some(every, ["gen_ai.output.messages", "output", "final_result"])
    hidden = sum(1 for a in llm if a.get("query_source_safe") not in (None, "sdk"))
    return {
        "spans / run": f"{len(sp) / n_runs:.1f}",
        "tokens on model spans": yes(some(llm, ["gen_ai.usage.input_tokens", "input_tokens"])),
        "TTFT on model spans": yes(some(llm, ["gen_ai.response.time_to_first_chunk",
                                              "gen_ai.client.operation.time_to_first_chunk", "ttft_ms"])),
        "tool arguments": yes(some([a for _, a in tool], ["gen_ai.tool.call.arguments", "input"])),
        "tool results": yes(some([a for _, a in tool], ["gen_ai.tool.call.result", "output"])),
        "tool error on span": yes(any(s.get("status", {}).get("code") == 2 or "error" in a or "error.type" in a
                                      for s, a in tool)),
        "prompt / response content": f"{yes(prompt)} / {yes(response)}",
        "hidden model calls": str(hidden),
        "OTel logs": str(logs.get(fw, 0)),
        "OTel metrics": str(len(metrics.get(fw, set()))),
        "GenAI semconv span names": yes(any(s["name"].startswith(("chat ", "execute_tool", "invoke_agent"))
                                            for s in sp)),
    }


def overhead(sp):
    traces = collections.defaultdict(list)
    for s in sp:
        traces[s["traceId"]].append(s)
    dur = lambda s: (int(s["endTimeUnixNano"]) - int(s["startTimeUnixNano"])) / 1e6  # noqa: E731
    out = []
    for t in traces.values():
        roots = [s for s in t if not s.get("parentSpanId")]
        if roots:
            out.append(max(dur(s) for s in roots) - sum(dur(s) for s in t if s["name"].startswith(LLM_SPAN)))
    return out


def main():
    runs, procs = load_runs()
    by = collections.defaultdict(list)
    for r in runs:
        by[(r["scenario"], r["framework"])].append(r)
    scenarios = list(dict.fromkeys(r["scenario"] for r in runs))
    frameworks = [fw for fw in ORDER if any(r["framework"] == fw for r in runs)]
    out = ["# Agent framework benchmark", "",
           f"Model `{json.loads((HERE / 'scenarios.json').read_text())['model']}` for every framework. "
           f"{len(runs)} measured runs in interleaved rounds. Latency is wall time for one full agent run, "
           "TTFT is time to the first streamed model delta.", ""]

    out += ["## Summary", "", "| Framework | Pass | p50 latency | p50 TTFT | Input tok / run | Output tok / run "
            "| Requests / run | Startup | Peak RSS |", "|---|---|---|---|---|---|---|---|---|"]
    for fw in frameworks:
        rs = [r for r in runs if r["framework"] == fw]
        p = procs.get(fw, {})
        rss = p.get("rss_mb", 0) + p.get("child_rss_mb", 0)
        out.append(
            f"| **{fw}** | {sum(r['ok'] for r in rs)}/{len(rs)} "
            f"| {fmt(pct([r['latency_ms'] for r in rs], .5), ' ms')} "
            f"| {fmt(pct([r['ttft_ms'] for r in rs if r['ttft_ms']], .5), ' ms')} "
            f"| {fmt(statistics.mean(r['input_tokens'] for r in rs))} "
            f"| {fmt(statistics.mean(r['output_tokens'] for r in rs))} "
            f"| {statistics.mean(r['model_requests'] for r in rs):.1f} "
            f"| {fmt(p.get('init_ms'), ' ms')} | {fmt(rss, ' MB')} |")

    out += ["", "## Latency by scenario (p50 / max, ms)", "",
            "| Scenario | " + " | ".join(frameworks) + " |", "|---|" + "---|" * len(frameworks)]
    for sc in scenarios:
        cells = []
        for fw in frameworks:
            lat = [r["latency_ms"] for r in by[(sc, fw)]]
            cells.append(f"{fmt(pct(lat, .5))} / {fmt(max(lat) if lat else None)}")
        out.append(f"| {sc} | " + " | ".join(cells) + " |")

    out += ["", "## Pass rate and tokens by scenario (pass, mean input/output tokens)", "",
            "| Scenario | " + " | ".join(frameworks) + " |", "|---|" + "---|" * len(frameworks)]
    for sc in scenarios:
        cells = []
        for fw in frameworks:
            rs = by[(sc, fw)]
            if not rs:
                cells.append("n/a")
                continue
            cells.append(f"{sum(r['ok'] for r in rs)}/{len(rs)}, "
                         f"{statistics.mean(r['input_tokens'] for r in rs):,.0f}/"
                         f"{statistics.mean(r['output_tokens'] for r in rs):,.0f}")
        out.append(f"| {sc} | " + " | ".join(cells) + " |")

    failures = [r for r in runs if not r["ok"]]
    if failures:
        out += ["", "## Failures", "", "| Framework | Scenario | Run | Reason |", "|---|---|---|---|"]
        seen = set()
        for r in failures:
            reason = (r["error"] or r["reason"]).replace("|", "/")[:120]
            key = (r["framework"], r["scenario"], reason)
            if key in seen:
                continue
            seen.add(key)
            n = sum(1 for f in failures if (f["framework"], f["scenario"]) == key[:2])
            out.append(f"| {r['framework']} | {r['scenario']} | {n}x | {reason} |")

    all_runs = collections.Counter()
    for path in (RESULTS / "raw").glob("*.jsonl"):
        for line in path.read_text().splitlines():
            rec = json.loads(line)
            if rec["type"] == "run":
                all_runs[rec["framework"]] += 1
    spans, logs, metrics = load_otel()
    obs = {fw: observability(fw, all_runs, spans, logs, metrics) for fw in frameworks}
    out += ["", "## Framework overhead (from traces)", "",
            "Root span time minus model-call time, per run. This removes API speed from the comparison. "
            "The Claude Agent SDK's spans overlap with a background title request, so its in-process "
            "number is not meaningful; its startup cost shows up as wall time minus the others on `plain`.", "",
            "| Framework | p50 | p90 |", "|---|---|---|"]
    for fw in frameworks:
        o = overhead(spans.get(fw, []))
        if fw == "claude-agent-sdk" or not o:
            plain = [r["latency_ms"] for r in runs if r["framework"] == fw and r["scenario"] == "plain"]
            others = [r["latency_ms"] for r in runs if r["framework"] != fw and r["scenario"] == "plain"]
            out.append(f"| {fw} | ~{fmt(pct(plain, .5) - pct(others, .5), ' ms')} per query (CLI subprocess) | n/a |")
        else:
            out.append(f"| {fw} | {fmt(pct(o, .5), ' ms')} | {fmt(pct(o, .9), ' ms')} |")
    rows = list(next(iter(obs.values())).keys())
    out += ["", "## Observability (from the shared OTel collector, warmups included)", "",
            "| Signal | " + " | ".join(frameworks) + " |", "|---|" + "---|" * len(frameworks)]
    for row in rows:
        out.append(f"| {row} | " + " | ".join(obs[fw][row] for fw in frameworks) + " |")

    out += ["", "### Span names", ""]
    for fw in frameworks:
        names = collections.Counter(s["name"] for s in spans.get(fw, []))
        out.append(f"- **{fw}**: " + ", ".join(f"`{n}` ({c})" for n, c in names.most_common(8)))

    (HERE / "results.md").write_text("\n".join(out) + "\n")
    print("\n".join(out))


if __name__ == "__main__":
    main()
