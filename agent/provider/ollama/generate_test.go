package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestGenerateReportsTruncation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		resp          GenerateResponse
		wantText      string
		wantTruncated bool
	}{
		{"complete", GenerateResponse{Response: "answer", Done: true, DoneReason: "stop"}, "answer", false},
		{"cut off by num_predict", GenerateResponse{Response: `{"entities": [`, Done: true, DoneReason: "length", EvalCount: 64}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.resp)
			}))
			defer server.Close()
			c := must.Get(NewClient(Config{Host: server.URL, Model: "qwen3:4b"}))
			text, err := c.Generate(context.Background(), "extract")
			if errors.Is(err, types.ErrResponseTruncated) != tc.wantTruncated || text != tc.wantText {
				t.Fatalf("Generate = %q, %v", text, err)
			}
			var te *types.ResponseTruncatedError
			if tc.wantTruncated && (!errors.As(err, &te) || te.OutputTokens != 64) {
				t.Fatalf("truncation detail = %+v", te)
			}
		})
	}
}
