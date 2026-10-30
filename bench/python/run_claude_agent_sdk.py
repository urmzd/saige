"""Claude Agent SDK runner.

The SDK drives the Claude Code CLI as a subprocess. Built-in tools, user and
project settings, and the default Claude Code system prompt are switched off
so the model sees the same prompt and tools as the other runners.

Tracing: the CLI's own OpenTelemetry export, enabled through environment
variables passed to the subprocess.
"""

import time

T0 = time.perf_counter()

import asyncio  # noqa: E402
import json  # noqa: E402
import os  # noqa: E402
import tempfile  # noqa: E402

from claude_agent_sdk import (  # noqa: E402
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    ToolUseBlock,
    create_sdk_mcp_server,
    query,
    tool,
)
from claude_agent_sdk.types import StreamEvent  # noqa: E402

import common  # noqa: E402

FRAMEWORK = "claude-agent-sdk"
SERVER = "bench"
PARAMS = {
    "get_weather": {"city": str},
    "get_population": {"city": str},
    "multiply": {"a": float, "b": float},
    "get_stock": {"symbol": str},
}


def wrap(name: str):
    func = common.TOOL_FUNCS[name]

    @tool(name, common.TOOLS[name]["description"], PARAMS[name])
    async def handler(args: dict) -> dict:
        return {"content": [{"type": "text", "text": func(**args)}]}

    return handler


TOOLS = {name: wrap(name) for name in common.TOOL_FUNCS}
WORKDIR = tempfile.mkdtemp(prefix="bench-claude-")
OTEL_ENV = {
    "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
    "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
    "OTEL_TRACES_EXPORTER": "otlp",
    "OTEL_METRICS_EXPORTER": "otlp",
    "OTEL_LOGS_EXPORTER": "otlp",
    "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
    "OTEL_EXPORTER_OTLP_ENDPOINT": os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"),
    "OTEL_RESOURCE_ATTRIBUTES": f"service.name={FRAMEWORK}",
    "OTEL_METRIC_EXPORT_INTERVAL": "1000",
    "OTEL_LOGS_EXPORT_INTERVAL": "1000",
    "OTEL_TRACES_EXPORT_INTERVAL": "1000",
}


def options(scenario: dict) -> ClaudeAgentOptions:
    names = scenario["tools"]
    servers = {}
    if names:
        servers[SERVER] = create_sdk_mcp_server(SERVER, tools=[TOOLS[n] for n in names])
    return ClaudeAgentOptions(
        model=common.SPEC["model"],
        system_prompt=common.SPEC["system_prompt"],
        tools=[],
        allowed_tools=[f"mcp__{SERVER}__{n}" for n in names],
        mcp_servers=servers,
        setting_sources=[],
        skills=[],
        cwd=WORKDIR,
        max_turns=10,
        include_partial_messages=True,
        output_format={"type": "json_schema", "schema": scenario["output_schema"]}
        if "output_schema" in scenario else None,
        env={**OTEL_ENV, "CLAUDE_CODE_MAX_OUTPUT_TOKENS": str(common.SPEC["max_tokens"])},
    )


async def run_once(scenario: dict, run: int, warmup: bool) -> common.Record:
    rec = common.Record(FRAMEWORK, scenario["id"], run, warmup)
    opts = options(scenario)
    start = common.now()
    try:
        async for msg in query(prompt=scenario["prompt"], options=opts):
            if isinstance(msg, StreamEvent):
                kind = msg.event.get("type")
                if rec.ttft_ms is None and kind == "content_block_delta":
                    rec.ttft_ms = common.ms(start)
                elif kind == "message_start":
                    rec.model_requests += 1
            elif isinstance(msg, AssistantMessage):
                rec.tool_calls += sum(isinstance(b, ToolUseBlock) for b in msg.content)
            elif isinstance(msg, ResultMessage):
                if msg.structured_output is not None:
                    rec.answer = json.dumps(msg.structured_output)
                else:
                    rec.answer = msg.result or ""
                usage = msg.usage or {}
                rec.input_tokens = (usage.get("input_tokens", 0)
                                    + usage.get("cache_read_input_tokens", 0)
                                    + usage.get("cache_creation_input_tokens", 0))
                rec.output_tokens = usage.get("output_tokens", 0)
                rec.cache_read_tokens = usage.get("cache_read_input_tokens", 0)
                rec.extra = {"num_turns": msg.num_turns, "duration_api_ms": msg.duration_api_ms,
                             "cost_usd": msg.total_cost_usd, "is_error": msg.is_error}
        rec.ok, rec.reason = common.check(scenario, rec.answer, rec.tool_calls)
    except Exception as exc:
        rec.error = f"{type(exc).__name__}: {exc}"
    rec.latency_ms = common.ms(start)
    return rec


async def main() -> None:
    args = common.parse_args(FRAMEWORK)
    options(common.SPEC["scenarios"][0])
    writer = common.Writer(args.out, FRAMEWORK, common.ms(T0))
    for scenario in common.selected(args):
        for i in range(args.warmup + args.runs):
            writer.write(await run_once(scenario, i - args.warmup, i < args.warmup))
    writer.close()


if __name__ == "__main__":
    asyncio.run(main())
