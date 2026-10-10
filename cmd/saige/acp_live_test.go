package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

// TestACPLive runs saige acp as a client would: the built binary on
// stdio, a definition on a real model, one prompt.
func TestACPLive(t *testing.T) {
	if os.Getenv("SAIGE_LIVE") != "1" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("set SAIGE_LIVE=1 and ANTHROPIC_API_KEY to call the provider")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "saige")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	agents := filepath.Join(dir, "agents")
	writeFile(t, filepath.Join(agents, "pong.agent.md"), definitionFile("pong", "1.0.0",
		"model: anthropic/claude-haiku-5-5\n", "Answer every message with exactly the word PONG."))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "acp", "--agent", "pong", "--agents-dir", agents)
	cmd.Dir = dir
	cmd.Stderr = io.Discard
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })

	client := &acpTestClient{answer: "allow_once"}
	conn := acp.NewClientSideConnection(client, stdin, stdout)
	conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: dir, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: sess.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("ping")}})
	if err != nil {
		t.Fatal(err)
	}
	updates, _ := client.snapshot()
	if res.StopReason != acp.StopReasonEndTurn || !strings.Contains(agentText(updates), "PONG") {
		t.Fatalf("stop %s, text %q", res.StopReason, agentText(updates))
	}
}
