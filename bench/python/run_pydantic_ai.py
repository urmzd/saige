"""Pydantic AI runner. Tracing: native OpenTelemetry via Agent.instrument_all()."""

import time

T0 = time.perf_counter()

import asyncio  # noqa: E402
import json  # noqa: E402

from pydantic_ai import (  # noqa: E402
    Agent,
    AgentRunResultEvent,
    FunctionToolCallEvent,
    PartDeltaEvent,
    PartStartEvent,
    Tool,
)
from pydantic_ai.output import StructuredDict  # noqa: E402

import common  # noqa: E402

FRAMEWORK = "pydantic-ai"


def build(scenario: dict) -> Agent:
    kwargs = {}
    if "output_schema" in scenario:
        kwargs["output_type"] = StructuredDict(scenario["output_schema"], name="person")
    return Agent(
        f"anthropic:{common.SPEC['model']}",
        instructions=common.SPEC["system_prompt"],
        tools=[Tool(common.TOOL_FUNCS[name], description=common.TOOLS[name]["description"])
               for name in scenario["tools"]],
        model_settings={"max_tokens": common.SPEC["max_tokens"]},
        **kwargs,
    )


async def run_once(scenario: dict, run: int, warmup: bool) -> common.Record:
    rec = common.Record(FRAMEWORK, scenario["id"], run, warmup)
    agent = build(scenario)
    start = common.now()
    try:
        async with agent.run_stream_events(scenario["prompt"]) as events:
            async for event in events:
                if rec.ttft_ms is None and isinstance(event, (PartStartEvent, PartDeltaEvent)):
                    rec.ttft_ms = common.ms(start)
                elif isinstance(event, FunctionToolCallEvent):
                    rec.tool_calls += 1
                elif isinstance(event, AgentRunResultEvent):
                    output = event.result.output
                    rec.answer = output if isinstance(output, str) else json.dumps(output)
                    usage = event.result.usage
                    rec.input_tokens = usage.input_tokens
                    rec.output_tokens = usage.output_tokens
                    rec.cache_read_tokens = usage.cache_read_tokens
                    rec.model_requests = usage.requests
        rec.ok, rec.reason = common.check(scenario, rec.answer, rec.tool_calls)
    except Exception as exc:  # the run itself failed
        rec.error = f"{type(exc).__name__}: {exc}"
    rec.latency_ms = common.ms(start)
    return rec


async def main() -> None:
    args = common.parse_args(FRAMEWORK)
    provider = common.setup_otel(FRAMEWORK)
    Agent.instrument_all()
    build(common.SPEC["scenarios"][0])
    writer = common.Writer(args.out, FRAMEWORK, common.ms(T0))
    for scenario in common.selected(args):
        for i in range(args.warmup + args.runs):
            writer.write(await run_once(scenario, i - args.warmup, i < args.warmup))
    writer.close()
    provider.force_flush()


if __name__ == "__main__":
    asyncio.run(main())
