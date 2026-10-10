package openai

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/types"
)

// toOpenAIMessages maps a conversation to Chat Completions messages. Parts
// map natively or the request is rejected before it is sent:
//
//   - text: a text part (system text becomes a system message);
//   - image (jpeg, png, gif, webp): image_url with an https URL or a data:
//     URI, and the part's detail;
//   - document (PDF only): a file part by file_id or inline file_data;
//   - audio (wav, mp3, inline only): input_audio, for audio-capable models;
//   - tool result: a tool message with the text and JSON output. Tool
//     messages carry text only, so a result with media is rejected;
//   - assistant refusal: the refusal field; assistant audio: audio.id while
//     the server copy lives, else its transcript as text.
//
// Video, opaque files, other document types, and media the surface cannot
// reach (a gs:// URI, another vendor's file, an unresolved workspace
// reference) fail with an error matching types.ErrModalityUnsupported or
// types.ErrMediaUnavailable that names the part.
func toOpenAIMessages(msgs []types.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for mi, m := range msgs {
		var err error
		switch v := m.(type) {
		case types.SystemMessage:
			out, err = appendChatSystem(out, mi, v)
		case types.UserMessage:
			out, err = appendChatUser(out, mi, v)
		case types.AssistantMessage:
			out, err = appendChatAssistant(out, mi, v)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func appendChatSystem(out []openai.ChatCompletionMessageParamUnion, mi int, v types.SystemMessage) ([]openai.ChatCompletionMessageParamUnion, error) {
	var text []string
	var results []indexedResult
	for pi, p := range v.Parts {
		switch bc := p.(type) {
		case types.TextPart:
			text = append(text, bc.Text)
		case types.ToolResultPart:
			results = append(results, indexedResult{pi, bc})
		default:
			if !types.IsMetadata(p) {
				return nil, rejectPart(surfaceChat, topLevel(mi, pi), p, "not valid in a system message")
			}
		}
	}
	if len(text) > 0 {
		out = append(out, openai.SystemMessage(strings.Join(text, "")))
	}
	return appendChatToolResults(out, mi, results)
}

func appendChatUser(out []openai.ChatCompletionMessageParamUnion, mi int, v types.UserMessage) ([]openai.ChatCompletionMessageParamUnion, error) {
	var parts []openai.ChatCompletionContentPartUnionParam
	var results []indexedResult
	for pi, p := range v.Parts {
		if tr, ok := p.(types.ToolResultPart); ok {
			results = append(results, indexedResult{pi, tr})
			continue
		}
		if types.IsMetadata(p) {
			continue
		}
		cp, err := chatContentPart(topLevel(mi, pi), p)
		if err != nil {
			return nil, err
		}
		parts = append(parts, cp)
	}
	// Tool messages answer the previous assistant turn's calls, so they
	// come before the user's own content.
	out, err := appendChatToolResults(out, mi, results)
	if err != nil {
		return nil, err
	}
	switch {
	case len(parts) == 1 && parts[0].OfText != nil:
		out = append(out, openai.UserMessage(parts[0].OfText.Text))
	case len(parts) > 0:
		out = append(out, openai.UserMessage(parts))
	}
	return out, nil
}

// chatContentPart maps one user part to a content part.
func chatContentPart(path types.PartPath, p types.Part) (openai.ChatCompletionContentPartUnionParam, error) {
	var zero openai.ChatCompletionContentPartUnionParam
	switch v := p.(type) {
	case types.TextPart:
		return openai.TextContentPart(v.Text), nil
	case types.ImagePart:
		if v.Source.MediaType != "" && !isImageType(v.Source.MediaType) {
			return zero, rejectPart(surfaceChat, path, p, "image types are jpeg, png, gif and webp")
		}
		loc, err := pickSource(surfaceChat, path, p, v.Source, types.SourceURI, types.SourceInline)
		if err != nil {
			return zero, err
		}
		url := loc.uri
		if loc.kind == types.SourceInline {
			if v.Source.MediaType == "" {
				return zero, rejectPart(surfaceChat, path, p, "inline image bytes need a media type")
			}
			url = dataURI(v.Source.MediaType, loc.data)
		}
		return openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: url, Detail: v.Detail}), nil
	case types.DocumentPart:
		if baseType(v.Source.MediaType) != types.MediaPDF {
			return zero, rejectPart(surfaceChat, path, p, "documents are PDF only on this surface; the Responses API takes other types")
		}
		loc, err := pickSource(surfaceChat, path, p, v.Source, types.SourceFile, types.SourceInline)
		if err != nil {
			return zero, err
		}
		if loc.kind == types.SourceFile {
			return openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{FileID: openai.String(loc.fileID)}), nil
		}
		return openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
			FileData: openai.String(dataURI(v.Source.MediaType, loc.data)),
			Filename: openai.String(filename(v.Source, defaultPDFName)),
		}), nil
	case types.AudioPart:
		format, ok := audioInputFormat(v.Source.MediaType, v.Format)
		if !ok {
			return zero, rejectPart(surfaceChat, path, p, "audio input is wav or mp3")
		}
		loc, err := pickSource(surfaceChat, path, p, v.Source, types.SourceInline)
		if err != nil {
			return zero, err
		}
		return openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{
			Data: base64.StdEncoding.EncodeToString(loc.data), Format: format,
		}), nil
	case types.VideoPart:
		return zero, rejectPart(surfaceChat, path, p, "video input is not supported")
	case types.FilePart:
		return zero, rejectPart(surfaceChat, path, p, "opaque files are not supported; send a PDF as a document part")
	}
	return zero, rejectPart(surfaceChat, path, p, "not supported in a user message")
}

// indexedResult is a tool result with its part index, for error paths.
type indexedResult struct {
	part int
	tr   types.ToolResultPart
}

// appendChatToolResults appends one tool message per result. OpenAI requires
// the tool messages answering one assistant turn to be contiguous.
func appendChatToolResults(out []openai.ChatCompletionMessageParamUnion, mi int, results []indexedResult) ([]openai.ChatCompletionMessageParamUnion, error) {
	for _, r := range results {
		for ni, p := range r.tr.Parts {
			switch p.(type) {
			case types.TextPart, types.JSONPart:
			default:
				return nil, rejectPart(surfaceChat, types.PartPath{Message: mi, Part: r.part, Nested: ni}, p,
					"Chat Completions tool messages carry text only; use the Responses API for media tool output")
			}
		}
		out = append(out, openai.ToolMessage(toolResultText(r.tr), r.tr.CallID))
	}
	return out, nil
}

// toolResultText is the text of a tool result, marked when it is an error.
func toolResultText(tr types.ToolResultPart) string {
	text := tr.Text()
	if tr.IsError {
		text = "[TOOL ERROR] " + text
	}
	return text
}

func appendChatAssistant(out []openai.ChatCompletionMessageParamUnion, mi int, v types.AssistantMessage) ([]openai.ChatCompletionMessageParamUnion, error) {
	var text, refusal []string
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
	var audioID string
	for pi, p := range v.Parts {
		switch bc := p.(type) {
		case types.TextPart:
			text = append(text, bc.Text)
		case types.ToolCallPart:
			argsJSON, _ := json.Marshal(bc.Arguments)
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: bc.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      bc.Name,
						Arguments: string(argsJSON),
					},
				},
			})
		case types.RefusalPart:
			refusal = append(refusal, bc.Text)
		case types.AudioOutPart:
			// A server copy is referenced by ID while it lives; after
			// that the transcript is what the model said.
			live := bc.VendorID != "" && (bc.ExpiresAt.IsZero() || time.Now().Before(bc.ExpiresAt))
			switch {
			case live && audioID == "":
				audioID = bc.VendorID
			case bc.Transcript != "":
				text = append(text, bc.Transcript)
			default:
				return nil, unavailablePart(surfaceChat, topLevel(mi, pi), p, "the audio's server copy expired and it has no transcript")
			}
		case types.ImageOutPart, types.VideoOutPart:
			return nil, rejectPart(surfaceChat, topLevel(mi, pi), p, "assistant media cannot be sent back on this surface")
		}
		// Thinking, citations and server tool parts have no Chat
		// Completions input form: the API returns no reasoning and runs
		// no server tools, and citations annotate the text already sent.
	}
	msg := openai.AssistantMessage(strings.Join(text, ""))
	if len(toolCalls) > 0 {
		msg.OfAssistant.ToolCalls = toolCalls
	}
	if len(refusal) > 0 {
		msg.OfAssistant.Refusal = openai.String(strings.Join(refusal, ""))
	}
	if audioID != "" {
		msg.OfAssistant.Audio = openai.ChatCompletionAssistantMessageParamAudio{ID: audioID}
	}
	return append(out, msg), nil
}
