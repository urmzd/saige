// Command saige-bench runs the shared benchmark scenarios on saige and writes
// one JSON record per run, in the same shape as the Python runners.
//
// Tracing: saige's own OpenTelemetry option (agent/otel.WithTracing),
// exported over OTLP/HTTP to the local collector.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	gootel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/otel"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/types"
)

const framework = "saige"

var start = time.Now()

type toolSpec struct {
	Description string         `json:"description"`
	Data        map[string]int `json:"data"`
	Error       string         `json:"error"`
}

type scenario struct {
	ID           string          `json:"id"`
	Prompt       string          `json:"prompt"`
	Tools        []string        `json:"tools"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Expect       struct {
		Contains     string         `json:"contains"`
		Digits       string         `json:"digits"`
		JSON         map[string]any `json:"json"`
		ToolCallsMin int            `json:"tool_calls_min"`
	} `json:"expect"`
}

type spec struct {
	Model        string              `json:"model"`
	SystemPrompt string              `json:"system_prompt"`
	MaxTokens    int64               `json:"max_tokens"`
	Tools        map[string]toolSpec `json:"tools"`
	Scenarios    []scenario          `json:"scenarios"`
}

type record struct {
	Type            string   `json:"type"`
	Framework       string   `json:"framework"`
	Scenario        string   `json:"scenario"`
	Run             int      `json:"run"`
	Warmup          bool     `json:"warmup"`
	OK              bool     `json:"ok"`
	Reason          string   `json:"reason"`
	LatencyMS       float64  `json:"latency_ms"`
	TTFTMS          *float64 `json:"ttft_ms"`
	InputTokens     int      `json:"input_tokens"`
	OutputTokens    int      `json:"output_tokens"`
	CacheReadTokens int      `json:"cache_read_tokens"`
	ModelRequests   int      `json:"model_requests"`
	ToolCalls       int      `json:"tool_calls"`
	Answer          string   `json:"answer"`
	Error           string   `json:"error"`
}

type cityIn struct {
	City string `json:"city" description:"City name"`
}

type multiplyIn struct {
	A float64 `json:"a" description:"First number"`
	B float64 `json:"b" description:"Second number"`
}

type symbolIn struct {
	Symbol string `json:"symbol" description:"Ticker symbol"`
}

// Person mirrors the structured scenario's schema.
type Person struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
	City string `json:"city"`
}

func tools(s spec) map[string]types.Tool {
	lookup := func(name, unit string) types.Tool {
		t := s.Tools[name]
		return agent.Func(name, t.Description, func(_ agent.RunContext[agent.NoDeps], in cityIn) (string, error) {
			v, ok := t.Data[in.City]
			if !ok {
				return "unknown city " + in.City, nil
			}
			return strconv.Itoa(v) + unit, nil
		})
	}
	return map[string]types.Tool{
		"get_weather":    lookup("get_weather", "C"),
		"get_population": lookup("get_population", ""),
		"multiply": agent.Func("multiply", s.Tools["multiply"].Description,
			func(_ agent.RunContext[agent.NoDeps], in multiplyIn) (string, error) {
				return strconv.FormatFloat(in.A*in.B, 'f', -1, 64), nil
			}),
		"get_stock": agent.Func("get_stock", s.Tools["get_stock"].Description,
			func(_ agent.RunContext[agent.NoDeps], in symbolIn) (string, error) {
				return "", errors.New(s.Tools["get_stock"].Error)
			}),
	}
}

func check(sc scenario, answer string, toolCalls int) (bool, string) {
	e := sc.Expect
	if toolCalls < e.ToolCallsMin {
		return false, fmt.Sprintf("tool_calls %d < %d", toolCalls, e.ToolCallsMin)
	}
	if e.Contains != "" && !strings.Contains(strings.ToLower(answer), strings.ToLower(e.Contains)) {
		return false, "missing " + strconv.Quote(e.Contains)
	}
	if e.Digits != "" && !strings.Contains(regexp.MustCompile(`\D`).ReplaceAllString(answer, ""), e.Digits) {
		return false, "missing digits " + e.Digits
	}
	if e.JSON != nil {
		var got map[string]any
		if err := json.Unmarshal([]byte(answer), &got); err != nil {
			return false, "answer is not JSON"
		}
		for k, want := range e.JSON {
			have := got[k]
			if ws, ok := want.(string); ok {
				hs, _ := have.(string)
				if !strings.Contains(strings.ToLower(hs), strings.ToLower(ws)) {
					return false, fmt.Sprintf("%s=%v", k, have)
				}
			} else if fmt.Sprint(have) != fmt.Sprint(want) {
				return false, fmt.Sprintf("%s=%v", k, have)
			}
		}
	}
	return true, ""
}

func fill(rec *record, t agent.Transcript) {
	if t.TTFT > 0 {
		v := float64(t.TTFT.Microseconds()) / 1000
		if rec.TTFTMS == nil {
			rec.TTFTMS = &v
		}
	}
	rec.InputTokens += t.Usage.PromptTokens
	rec.OutputTokens += t.Usage.CompletionTokens
	rec.CacheReadTokens += t.Usage.CachedPromptTokens
	rec.ModelRequests += t.Turns
	rec.ToolCalls += len(t.ToolCalls)
}

func runOnce(ctx context.Context, s spec, all map[string]types.Tool, provider types.Provider, sc scenario, run int, warmup bool) record {
	rec := record{Type: "run", Framework: framework, Scenario: sc.ID, Run: run, Warmup: warmup}
	reg := types.NewToolRegistry()
	for _, name := range sc.Tools {
		reg.Register(all[name])
	}
	a, err := agent.New(agent.Config{Name: "bench", SystemPrompt: s.SystemPrompt, Provider: provider, Tools: reg},
		otel.WithTracing(otel.Config{ServiceName: framework, CaptureToolContent: true}))
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	input := []types.Message{types.UserMsg(types.Text(sc.Prompt))}
	began := time.Now()
	if sc.OutputSchema != nil {
		out, res, err := agent.Structured(ctx, a, input, agent.OutputSpec[Person]{Repair: 1})
		if res != nil {
			for _, t := range res.Transcripts {
				fill(&rec, t)
			}
		}
		if err != nil {
			rec.Error = err.Error()
		} else {
			b, _ := json.Marshal(out)
			rec.Answer = string(b)
		}
	} else {
		t, err := agent.Collect(a.Invoke(ctx, input), nil)
		fill(&rec, t)
		rec.Answer = t.Text
		if err != nil {
			rec.Error = err.Error()
		}
	}
	rec.LatencyMS = float64(time.Since(began).Microseconds()) / 1000
	if rec.Error == "" {
		rec.OK, rec.Reason = check(sc, rec.Answer, rec.ToolCalls)
	}
	if len(rec.Answer) > 300 {
		rec.Answer = rec.Answer[:300]
	}
	return rec
}

func main() {
	runs := flag.Int("runs", 5, "measured runs per scenario")
	warm := flag.Int("warmup", 1, "warmup runs per scenario")
	only := flag.String("scenarios", "", "comma-separated ids; empty runs all")
	out := flag.String("out", "../results/saige.jsonl", "output JSONL path")
	specPath := flag.String("spec", "../scenarios.json", "scenario file")
	flag.Parse()
	ctx := context.Background()

	raw, err := os.ReadFile(*specPath)
	if err != nil {
		log.Fatal(err)
	}
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		log.Fatal(err)
	}

	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(framework))))
	defer tp.Shutdown(ctx)
	gootel.SetTracerProvider(tp)

	provider, err := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: types.ModelID(s.Model)},
		anthropic.WithMaxTokens(s.MaxTokens))
	if err != nil {
		log.Fatal(err)
	}
	all := tools(s)
	initMS := float64(time.Since(start).Microseconds()) / 1000

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	ids := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id != "" {
			ids[id] = true
		}
	}
	for _, sc := range s.Scenarios {
		if len(ids) > 0 && !ids[sc.ID] {
			continue
		}
		for i := range *warm + *runs {
			rec := runOnce(ctx, s, all, provider, sc, i-*warm, i < *warm)
			_ = enc.Encode(rec)
			tag := fmt.Sprintf("#%d", rec.Run)
			if rec.Warmup {
				tag = "warm"
			}
			status := "ok  "
			if !rec.OK {
				status = "FAIL"
			}
			fmt.Printf("%-8s %-15s %-5s %s %7.0fms in=%d out=%d req=%d tools=%d %s%s\n", framework, sc.ID, tag, status,
				rec.LatencyMS, rec.InputTokens, rec.OutputTokens, rec.ModelRequests, rec.ToolCalls, rec.Reason, rec.Error)
		}
	}
	// Peak resident memory; ru_maxrss is in bytes on macOS.
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	_ = enc.Encode(map[string]any{"type": "process", "framework": framework, "init_ms": initMS,
		"rss_mb": float64(ru.Maxrss) / 1e6, "child_rss_mb": 0, "goos": runtime.GOOS})
}
