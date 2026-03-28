package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/google"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
)

var defaultModels = map[string]string{
	providerAnthropic: "claude-sonnet-4-6-20250514",
	providerOpenAI:    "gpt-4o",
	providerGoogle:    "gemini-2.0-flash",
	providerOllama:    "qwen3.5:4b",
}

var defaultEmbedModels = map[string]string{
	providerAnthropic: "",
	providerOpenAI:    "text-embedding-3-small",
	providerGoogle:    "text-embedding-004",
	providerOllama:    "nomic-embed-text",
}

// commonFlags holds flags shared by chat and ask commands.
type commonFlags struct {
	provider   *string
	model      *string
	system     *string
	ollamaHost *string
	baseURL    *string
	embedModel *string
	ragDB      *string
	kgDB       *string
}

// addCommonFlags registers provider and connection flags on a FlagSet.
func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	cf := &commonFlags{}
	cf.provider = fs.String("provider", envOr("SAIGE_PROVIDER", ""), "LLM provider (anthropic|openai|google|ollama)")
	cf.model = fs.String("model", "", "Model name (provider-specific default)")
	cf.system = fs.String("system", "You are a helpful assistant.", "System prompt")
	cf.ollamaHost = fs.String("ollama-host", envOr("OLLAMA_HOST", "http://localhost:11434"), "Ollama host URL")
	cf.baseURL = fs.String("base-url", "", "Custom API base URL (OpenAI-compatible)")
	cf.embedModel = fs.String("embed-model", "", "Embedding model name (provider-specific default)")
	cf.ragDB = fs.String("rag-db", envOr("SAIGE_RAG_DB", ""), "Postgres DSN for RAG tools")
	cf.kgDB = fs.String("kg-db", envOr("SAIGE_KG_DB", ""), "Postgres DSN for KG tools")
	return cf
}

// resolvedProvider returns the provider name, falling back to env then auto-detect.
func (cf *commonFlags) resolvedProvider() string {
	if *cf.provider != "" {
		return *cf.provider
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return providerAnthropic
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return providerOpenAI
	}
	if os.Getenv("GOOGLE_API_KEY") != "" {
		return providerGoogle
	}
	return providerOllama
}

// resolvedModel returns the model name, falling back to the provider default.
func (cf *commonFlags) resolvedModel() string {
	if *cf.model != "" {
		return *cf.model
	}
	return defaultModels[cf.resolvedProvider()]
}

// resolvedEmbedModel returns the embed model name, falling back to provider default.
func (cf *commonFlags) resolvedEmbedModel() string {
	if *cf.embedModel != "" {
		return *cf.embedModel
	}
	return defaultEmbedModels[cf.resolvedProvider()]
}

// resolveProvider creates a types.Provider from the resolved flags.
func resolveProvider(ctx context.Context, cf *commonFlags) (types.Provider, error) {
	name := cf.resolvedProvider()
	model := cf.resolvedModel()

	switch name {
	case providerOllama:
		client := ollama.NewClient(*cf.ollamaHost, model, cf.resolvedEmbedModel())
		return ollama.NewAdapter(client), nil

	case providerOpenAI:
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY is required for openai provider")
		}
		var opts []openai.Option
		if *cf.baseURL != "" {
			opts = append(opts, openai.WithBaseURL(*cf.baseURL))
		}
		return openai.NewAdapter(apiKey, model, opts...), nil

	case providerAnthropic:
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required for anthropic provider")
		}
		return anthropic.NewAdapter(apiKey, model), nil

	case providerGoogle:
		apiKey := os.Getenv("GOOGLE_API_KEY")
		if apiKey == "" {
			return nil, fmt.Errorf("GOOGLE_API_KEY is required for google provider")
		}
		return google.NewAdapter(ctx, apiKey, model)

	default:
		return nil, fmt.Errorf("unknown provider: %s", name)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
