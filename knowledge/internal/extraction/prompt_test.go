package extraction

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/knowledge/types"
)

func TestBuildExtractionPrompt_NoOntology(t *testing.T) {
	prompt := BuildExtractionPrompt("Alice works at Acme", nil)

	if !strings.Contains(prompt, "Alice works at Acme") {
		t.Error("prompt should contain the input text")
	}
	if !strings.Contains(prompt, "Extract entities") {
		t.Error("prompt should contain extraction instructions")
	}
	if strings.Contains(prompt, "Known entity types") {
		t.Error("prompt should not contain ontology section when nil")
	}
}

func TestBuildExtractionPrompt_EmptyOntology(t *testing.T) {
	ont := &types.Ontology{}
	prompt := BuildExtractionPrompt("test", ont)

	if strings.Contains(prompt, "Known entity types") {
		t.Error("prompt should not contain ontology section when types are empty")
	}
}

func TestBuildExtractionPrompt_WithOntology(t *testing.T) {
	ont := &types.Ontology{
		EntityTypes: []types.EntityTypeDef{
			{Name: "Person", Description: "A person"},
			{Name: "Organization", Description: "An org"},
		},
		RelationTypes: []types.RelationTypeDef{
			{Name: "works_at", Description: "Employment"},
			{Name: "knows", Description: "Acquaintance"},
		},
	}

	prompt := BuildExtractionPrompt("Alice works at Acme", ont)

	if !strings.Contains(prompt, "Person") {
		t.Error("prompt should contain entity type Person")
	}
	if !strings.Contains(prompt, "Organization") {
		t.Error("prompt should contain entity type Organization")
	}
	if !strings.Contains(prompt, "works_at") {
		t.Error("prompt should contain relation type works_at")
	}
	if !strings.Contains(prompt, "knows") {
		t.Error("prompt should contain relation type knows")
	}
	if !strings.Contains(prompt, "Known entity types: Person, Organization") {
		t.Error("prompt should list entity types comma-separated")
	}
	if !strings.Contains(prompt, "Known relation types: works_at, knows") {
		t.Error("prompt should list relation types comma-separated")
	}
}

func TestBuildExtractionPrompt_JSONFormat(t *testing.T) {
	prompt := BuildExtractionPrompt("test", nil)

	if !strings.Contains(prompt, `"entities"`) {
		t.Error("prompt should contain entities JSON key")
	}
	if !strings.Contains(prompt, `"relations"`) {
		t.Error("prompt should contain relations JSON key")
	}
}
