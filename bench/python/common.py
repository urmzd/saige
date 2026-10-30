"""Shared pieces for the Python runners: scenarios, tools, checks, records."""

from __future__ import annotations

import argparse
import json
import os
import re
import resource
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path

BENCH = Path(__file__).resolve().parent.parent
SPEC = json.loads((BENCH / "scenarios.json").read_text())
TOOLS = SPEC["tools"]


# Tool implementations. Every runner wraps these same functions.
def get_weather(city: str) -> str:
    temp = TOOLS["get_weather"]["data"].get(city)
    return f"{temp}C" if temp is not None else f"unknown city {city}"


def get_population(city: str) -> str:
    pop = TOOLS["get_population"]["data"].get(city)
    return str(pop) if pop is not None else f"unknown city {city}"


def multiply(a: float, b: float) -> str:
    product = a * b
    return str(int(product)) if float(product).is_integer() else str(product)


def get_stock(symbol: str) -> str:
    raise RuntimeError(TOOLS["get_stock"]["error"])


TOOL_FUNCS = {
    "get_weather": get_weather,
    "get_population": get_population,
    "multiply": multiply,
    "get_stock": get_stock,
}


def check(scenario: dict, answer: str, tool_calls: int) -> tuple[bool, str]:
    """Return whether the answer meets the scenario's expectation."""
    expect = scenario["expect"]
    if tool_calls < expect.get("tool_calls_min", 0):
        return False, f"tool_calls {tool_calls} < {expect['tool_calls_min']}"
    if "contains" in expect and expect["contains"].lower() not in answer.lower():
        return False, f"missing {expect['contains']!r}"
    if "digits" in expect and expect["digits"] not in re.sub(r"\D", "", answer):
        return False, f"missing digits {expect['digits']}"
    if "json" in expect:
        try:
            got = json.loads(answer) if isinstance(answer, str) else answer
        except json.JSONDecodeError:
            return False, "answer is not JSON"
        for key, want in expect["json"].items():
            have = got.get(key)
            if isinstance(want, str):
                if not isinstance(have, str) or want.lower() not in have.lower():
                    return False, f"{key}={have!r}"
            elif have != want:
                return False, f"{key}={have!r}"
    return True, ""


@dataclass
class Record:
    framework: str
    scenario: str
    run: int
    warmup: bool
    ok: bool = False
    reason: str = ""
    latency_ms: float = 0.0
    ttft_ms: float | None = None
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_tokens: int = 0
    model_requests: int = 0
    tool_calls: int = 0
    answer: str = ""
    error: str = ""
    extra: dict = field(default_factory=dict)


class Writer:
    def __init__(self, path: Path, framework: str, init_ms: float):
        self.path = path
        self.framework = framework
        self.init_ms = init_ms
        path.parent.mkdir(parents=True, exist_ok=True)
        self.fh = path.open("w")

    def write(self, rec: Record) -> None:
        rec.answer = rec.answer[:300]
        self.fh.write(json.dumps({"type": "run", **asdict(rec)}) + "\n")
        self.fh.flush()
        status = "ok  " if rec.ok else "FAIL"
        tag = "warm" if rec.warmup else f"#{rec.run}"
        print(f"{self.framework:8} {rec.scenario:15} {tag:5} {status} {rec.latency_ms:7.0f}ms "
              f"in={rec.input_tokens} out={rec.output_tokens} req={rec.model_requests} "
              f"tools={rec.tool_calls} {rec.reason or rec.error[:80]}", flush=True)

    def close(self) -> None:
        # Peak resident memory; ru_maxrss is in bytes on macOS. Children
        # covers subprocesses such as the Claude Code CLI.
        rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1e6
        children = resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss / 1e6
        self.fh.write(json.dumps({"type": "process", "framework": self.framework,
                                  "init_ms": self.init_ms, "rss_mb": rss,
                                  "child_rss_mb": children}) + "\n")
        self.fh.close()


def parse_args(framework: str) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=f"{framework} benchmark runner")
    p.add_argument("--runs", type=int, default=5)
    p.add_argument("--warmup", type=int, default=1)
    p.add_argument("--scenarios", default="", help="comma-separated ids; empty runs all")
    p.add_argument("--out", type=Path, default=BENCH / "results" / f"{framework}.jsonl")
    return p.parse_args()


def selected(args: argparse.Namespace) -> list[dict]:
    ids = [s for s in args.scenarios.split(",") if s]
    return [s for s in SPEC["scenarios"] if not ids or s["id"] in ids]


def setup_otel(service: str):
    """Point the OpenTelemetry SDK at the local collector."""
    from opentelemetry import trace
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
    provider = TracerProvider(resource=Resource.create({"service.name": service}))
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=f"{endpoint}/v1/traces")))
    trace.set_tracer_provider(provider)
    return provider


def now() -> float:
    return time.perf_counter()


def ms(start: float) -> float:
    return (time.perf_counter() - start) * 1000
