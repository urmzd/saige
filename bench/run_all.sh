#!/usr/bin/env bash
# Runs every framework over every scenario in interleaved rounds: each round
# runs all four frameworks once, rotating the order, so API latency drift
# during the run affects every framework equally.
# Needs ANTHROPIC_API_KEY and the collector from otel/collector.yaml on :4318.
set -euo pipefail
cd "$(dirname "$0")"
ROUNDS="${ROUNDS:-5}"
export OTEL_EXPORTER_OTLP_ENDPOINT="${OTEL_EXPORTER_OTLP_ENDPOINT:-http://localhost:4318}"
FRAMEWORKS=(saige pydantic_ai openai_agents claude_agent_sdk)

(cd saige && go build -o ../results/saige-bench .)
mkdir -p results/raw
run() {
  local fw="$1" out="results/raw/${1//_/-}-r$2.jsonl"
  if [ "$fw" = saige ]; then
    results/saige-bench --runs 1 --warmup 1 --spec scenarios.json --out "$out"
  else
    (cd python && uv run python "run_${fw}.py" --runs 1 --warmup 1 --out "../$out") 2>&1 | grep -v "connectors are disabled"
  fi
}
for ((r = 0; r < ROUNDS; r++)); do
  for ((i = 0; i < ${#FRAMEWORKS[@]}; i++)); do
    run "${FRAMEWORKS[$(((i + r) % ${#FRAMEWORKS[@]}))]}" "$r"
  done
done
