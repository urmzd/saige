package agent_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/internal/must"
)

// TestAIFuncLive makes one real call. It runs only with SAIGE_LIVE=1 and an
// OPENAI_API_KEY.
func TestAIFuncLive(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if os.Getenv("SAIGE_LIVE") != "1" || key == "" {
		t.Skip("set SAIGE_LIVE=1 and OPENAI_API_KEY to call the provider")
	}
	type in struct {
		Word string `json:"word"`
	}
	type out struct {
		Letters int    `json:"letters" description:"Number of letters in the word"`
		Upper   string `json:"upper" description:"The word in upper case"`
	}
	f, err := agent.AIFunc[in, out]("spell", "Spell a word", agent.AIConfig{
		Prompt:   "Count the letters of the word {{printf \"%q\" .Word}} and write it in upper case.",
		Provider: must.Get(openai.New(openai.Config{APIKey: key, Model: "gpt-6-luna"})),
		Repair:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got, err := f.Call(ctx, in{Word: "saige"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Upper != "SAIGE" || got.Letters != 5 {
		t.Fatalf("got %+v", got)
	}
	t.Logf("version %s, answer %+v", f.Version(), got)
}
