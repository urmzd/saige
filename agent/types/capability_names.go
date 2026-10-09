package types

// KnownCapabilities lists every Capability this SDK defines, sorted. Catalog
// loading uses it to reject a misspelled capability rather than store a flag
// nothing will ever check.
func KnownCapabilities() []Capability {
	return []Capability{
		CapAssistantPrefill, CapAutomaticPromptCache, CapCitations, CapCodeExecution,
		CapContextWindowOverride, CapEmbeddings, CapExplicitContextCache, CapFrequencyPenalty,
		CapMaxOutputTokens, CapParallelToolControl, CapParallelTools, CapPresencePenalty,
		CapPromptCacheMarkers, CapPromptCaching, CapReasoning, CapReasoningBudget,
		CapReasoningEffort, CapReasoningSignature, CapReasoningToggle, CapRemoteMCP,
		CapSafetySettings, CapSeed, CapServerTools, CapStopSequences, CapStreaming,
		CapStructuredOutput, CapSystemPrompt, CapTemperature, CapToolChoice, CapTools,
		CapTopK, CapTopP, CapWebSearch,
	}
}

// KnownMediaTypes lists every MediaType this SDK defines.
func KnownMediaTypes() []MediaType {
	return []MediaType{
		MediaJPEG, MediaPNG, MediaGIF, MediaWebP, MediaPDF, MediaCSV, MediaMP3, MediaWAV,
		MediaMP4, MediaDOCX, MediaXLSX, MediaPPTX, MediaHTML, MediaText, MediaJSON,
	}
}

// KnownServerToolKinds lists every ServerToolKind this SDK defines.
func KnownServerToolKinds() []ServerToolKind {
	return []ServerToolKind{ServerToolWebSearch, ServerToolCodeExecution, ServerToolRemoteMCP}
}
