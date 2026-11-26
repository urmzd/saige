"""OpenAI Agents SDK runner, reaching Claude through LiteLLM.

Tracing: the SDK's own traces go to OpenAI's dashboard. They are turned off
here and bridged to OpenTelemetry with logfire.instrument_openai_agents().
"""

import time

T0 = time.perf_counter()

import asyncio  # noqa: E402
import os  # noqa: E402

import logfire  # noqa: E402
from agents import (  # noqa: E402
    Agent,
    ModelSettings,
    Runner,
    function_tool,
    set_trace_processors,
)
from agents.extensions.models.litellm_model import LitellmModel  # noqa: E402
from pydantic import BaseModel  # noqa: E402

import common  # noqa: E402

FRAMEWORK = "openai-agents"


class Person(BaseModel):
    name: str
    age: int
    city: str


MODEL = LitellmModel(model=f"anthropic/{common.SPEC['model']}", api_key=os.environ["ANTHROPIC_API_KEY"])
TOOLS = {
    name: function_tool(func, description_override=common.TOOLS[name]["description"])
    for name, func in common.TOOL_FUNCS.items()
}


def build(scenario: dict) -> Agent:
    return Agent(
        name="bench",
        instructions=common.SPEC["system_prompt"],
        model=MODEL,
        model_settings=ModelSettings(max_tokens=common.SPEC["max_tokens"], include_usage=True),
        tools=[TOOLS[name] for name in scenario["tools"]],
        output_type=Person if "output_schema" in scenario else None,
    )


async def run_once(scenario: dict, run: int, warmup: bool) -> common.Record:
    rec = common.Record(FRAMEWORK, scenario["id"], run, warmup)
    agent = build(scenario)
    start = common.now()
    try:
        result = Runner.run_streamed(agent, scenario["prompt"])
        async for event in result.stream_events():
            if rec.ttft_ms is None and event.type == "raw_response_event":
                rec.ttft_ms = common.ms(start)
            elif event.type == "run_item_stream_event" and event.item.type == "tool_call_item":
                rec.tool_calls += 1
        output = result.final_output
        rec.answer = output.model_dump_json() if isinstance(output, BaseModel) else str(output)
        usage = result.context_wrapper.usage
        rec.input_tokens = usage.input_tokens
        rec.output_tokens = usage.output_tokens
        rec.cache_read_tokens = usage.input_tokens_details.cached_tokens or 0
        rec.model_requests = usage.requests
        rec.ok, rec.reason = common.check(scenario, rec.answer, rec.tool_calls)
    except Exception as exc:
        rec.error = f"{type(exc).__name__}: {exc}"
    rec.latency_ms = common.ms(start)
    return rec


async def main() -> None:
    args = common.parse_args(FRAMEWORK)
    set_trace_processors([])
    logfire.configure(service_name=FRAMEWORK, send_to_logfire=False, console=False)
    logfire.instrument_openai_agents()
    build(common.SPEC["scenarios"][0])
    writer = common.Writer(args.out, FRAMEWORK, common.ms(T0))
    for scenario in common.selected(args):
        for i in range(args.warmup + args.runs):
            writer.write(await run_once(scenario, i - args.warmup, i < args.warmup))
    writer.close()
    logfire.force_flush()


if __name__ == "__main__":
    asyncio.run(main())
