package google

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

func TestUsageByModality(t *testing.T) {
	u := usageOf(&genai.GenerateContentResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 1300, CandidatesTokenCount: 40, TotalTokenCount: 1340,
		PromptTokensDetails: []*genai.ModalityTokenCount{
			{Modality: genai.MediaModalityText, TokenCount: 42},
			{Modality: genai.MediaModalityImage, TokenCount: 258},
			{Modality: genai.MediaModalityAudio, TokenCount: 1000},
			{Modality: genai.MediaModalityUnspecified, TokenCount: 7},
		},
		CandidatesTokensDetails: []*genai.ModalityTokenCount{{Modality: genai.MediaModalityText, TokenCount: 40}},
	}})
	want := map[types.Modality]int{types.ModalityText: 42, types.ModalityImage: 258, types.ModalityAudio: 1000}
	if len(u.PromptByModality) != len(want) {
		t.Fatalf("prompt by modality = %v, want %v", u.PromptByModality, want)
	}
	for m, n := range want {
		if u.PromptByModality[m] != n {
			t.Fatalf("prompt by modality = %v, want %v", u.PromptByModality, want)
		}
	}
	if u.CompletionByModality[types.ModalityText] != 40 {
		t.Fatalf("completion by modality = %v", u.CompletionByModality)
	}
	if none := usageOf(&genai.GenerateContentResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 3}}); none.PromptByModality != nil {
		t.Fatalf("unreported details = %v, want nil", none.PromptByModality)
	}
}
