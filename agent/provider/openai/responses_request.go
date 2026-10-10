package openai

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/urmzd/saige/agent/types"
)

// toResponsesInput maps a conversation to Responses input items. Parts map
// natively or the request is rejected before it is sent:
//
//   - text: input_text (system text becomes a system message);
//   - image (jpeg, png, gif, webp): input_image by file_id, URL or data:
//     URI, with the part's detail;
//   - document and file: input_file by file_id, file_url or file_data, for
//     the types the API reads (PDF, text and code, office documents,
//     spreadsheets, presentations);
//   - tool result: function_call_output, a string when the output is text
//     and JSON, else a list of input_text, input_image and input_file;
//   - assistant reasoning this API produced (encrypted content): a
//     reasoning item; an assistant refusal: a refusal content item.
//
// Audio and video input, and audio, image or video output sent back, fail
// with an error matching types.ErrModalityUnsupported that names the part.
func toResponsesInput(msgs []types.Message) (responses.ResponseInputParam, error) {
	var out responses.ResponseInputParam
	for mi, m := range msgs {
		var err error
		switch v := m.(type) {
		case types.SystemMessage:
			out, err = appendResponsesSystem(out, mi, v)
		case types.UserMessage:
			out, err = appendResponsesUser(out, mi, v)
		case types.AssistantMessage:
			out, err = appendResponsesAssistant(out, mi, v)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func appendResponsesSystem(out responses.ResponseInputParam, mi int, v types.SystemMessage) (responses.ResponseInputParam, error) {
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
				return nil, rejectPart(surfaceResponses, topLevel(mi, pi), p, "not valid in a system message")
			}
		}
	}
	if len(text) > 0 {
		out = append(out, responses.ResponseInputItemParamOfMessage(strings.Join(text, ""), responses.EasyInputMessageRoleSystem))
	}
	return appendResponsesToolResults(out, mi, results)
}

func appendResponsesUser(out responses.ResponseInputParam, mi int, v types.UserMessage) (responses.ResponseInputParam, error) {
	var parts responses.ResponseInputMessageContentListParam
	var results []indexedResult
	for pi, p := range v.Parts {
		if tr, ok := p.(types.ToolResultPart); ok {
			results = append(results, indexedResult{pi, tr})
			continue
		}
		if types.IsMetadata(p) {
			continue
		}
		c, err := responsesContent(topLevel(mi, pi), p)
		if err != nil {
			return nil, err
		}
		parts = append(parts, c)
	}
	out, err := appendResponsesToolResults(out, mi, results)
	if err != nil {
		return nil, err
	}
	if len(parts) > 0 {
		out = append(out, responses.ResponseInputItemParamOfMessage(parts, responses.EasyInputMessageRoleUser))
	}
	return out, nil
}

// responsesContent maps one user part to message content.
func responsesContent(path types.PartPath, p types.Part) (responses.ResponseInputContentUnionParam, error) {
	var zero responses.ResponseInputContentUnionParam
	switch v := p.(type) {
	case types.TextPart:
		return inputText(v.Text), nil
	case types.ImagePart:
		img, err := responsesImage(path, p, v)
		if err != nil {
			return zero, err
		}
		return responses.ResponseInputContentUnionParam{OfInputImage: &responses.ResponseInputImageParam{
			FileID: img.FileID, ImageURL: img.ImageURL, Detail: responses.ResponseInputImageDetail(img.Detail),
		}}, nil
	case types.DocumentPart, types.FilePart:
		src, _ := types.SourceOf(p)
		f, err := responsesFile(path, p, src)
		if err != nil {
			return zero, err
		}
		return responses.ResponseInputContentUnionParam{OfInputFile: &responses.ResponseInputFileParam{
			FileID: f.FileID, FileURL: f.FileURL, FileData: f.FileData, Filename: f.Filename,
		}}, nil
	case types.AudioPart:
		return zero, rejectPart(surfaceResponses, path, p, "the Responses API takes no audio input; use Chat Completions with an audio model")
	case types.VideoPart:
		return zero, rejectPart(surfaceResponses, path, p, "video input is not supported")
	}
	return zero, rejectPart(surfaceResponses, path, p, "not supported in a user message")
}

// responsesImage maps an image to the fields input_image takes in a
// message and in tool output.
func responsesImage(path types.PartPath, p types.Part, v types.ImagePart) (responses.ResponseInputImageContentParam, error) {
	var img responses.ResponseInputImageContentParam
	if v.Source.MediaType != "" && !isImageType(v.Source.MediaType) {
		return img, rejectPart(surfaceResponses, path, p, "image types are jpeg, png, gif and webp")
	}
	loc, err := pickSource(surfaceResponses, path, p, v.Source, types.SourceFile, types.SourceURI, types.SourceInline)
	if err != nil {
		return img, err
	}
	switch loc.kind {
	case types.SourceFile:
		img.FileID = openai.String(loc.fileID)
	case types.SourceURI:
		img.ImageURL = openai.String(loc.uri)
	default:
		if v.Source.MediaType == "" {
			return img, rejectPart(surfaceResponses, path, p, "inline image bytes need a media type")
		}
		img.ImageURL = openai.String(dataURI(v.Source.MediaType, loc.data))
	}
	img.Detail = responses.ResponseInputImageContentDetail(v.Detail)
	if img.Detail == "" {
		img.Detail = responses.ResponseInputImageContentDetailAuto
	}
	return img, nil
}

// responsesFile maps a document or file to the fields input_file takes.
func responsesFile(path types.PartPath, p types.Part, src types.Source) (responses.ResponseInputFileContentParam, error) {
	var f responses.ResponseInputFileContentParam
	if !isResponsesFileType(src.MediaType) {
		return f, rejectPart(surfaceResponses, path, p, "input_file takes PDF, text and code, office documents, spreadsheets and presentations")
	}
	loc, err := pickSource(surfaceResponses, path, p, src, types.SourceFile, types.SourceURI, types.SourceInline)
	if err != nil {
		return f, err
	}
	switch {
	case loc.kind == types.SourceFile:
		f.FileID = openai.String(loc.fileID)
	case loc.kind == types.SourceURI && strings.HasPrefix(strings.ToLower(loc.uri), "data:"):
		f.FileData = openai.String(loc.uri)
		f.Filename = openai.String(filename(src, defaultFilename(src.MediaType)))
	case loc.kind == types.SourceURI:
		f.FileURL = openai.String(loc.uri)
	default:
		f.FileData = openai.String(dataURI(src.MediaType, loc.data))
		f.Filename = openai.String(filename(src, defaultFilename(src.MediaType)))
	}
	return f, nil
}

func inputText(text string) responses.ResponseInputContentUnionParam {
	return responses.ResponseInputContentUnionParam{OfInputText: &responses.ResponseInputTextParam{Text: text}}
}

// appendResponsesToolResults appends a function_call_output per result.
// Text and JSON output is sent as a string; output with media is sent as a
// list of content items, so images and files reach the model natively.
func appendResponsesToolResults(out responses.ResponseInputParam, mi int, results []indexedResult) (responses.ResponseInputParam, error) {
	for _, r := range results {
		item := responses.ResponseInputItemParamOfFunctionCallOutput(toolResultText(r.tr))
		if r.tr.HasMedia() {
			list, err := responsesToolOutput(mi, r)
			if err != nil {
				return nil, err
			}
			item.OfFunctionCallOutput.Output = responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfResponseFunctionCallOutputItemArray: list}
		}
		item.OfFunctionCallOutput.CallID = param.NewOpt(r.tr.CallID)
		out = append(out, item)
	}
	return out, nil
}

func responsesToolOutput(mi int, r indexedResult) (responses.ResponseFunctionCallOutputItemListParam, error) {
	var list responses.ResponseFunctionCallOutputItemListParam
	text := func(s string) {
		list = append(list, responses.ResponseFunctionCallOutputItemUnionParam{OfInputText: &responses.ResponseInputTextContentParam{Text: s}})
	}
	if r.tr.IsError {
		text("[TOOL ERROR]")
	}
	for ni, p := range r.tr.Parts {
		path := types.PartPath{Message: mi, Part: r.part, Nested: ni}
		switch v := p.(type) {
		case types.TextPart:
			text(v.Text)
		case types.JSONPart:
			text(string(v.JSON))
		case types.ImagePart:
			img, err := responsesImage(path, p, v)
			if err != nil {
				return nil, err
			}
			list = append(list, responses.ResponseFunctionCallOutputItemUnionParam{OfInputImage: &img})
		case types.DocumentPart, types.FilePart:
			src, _ := types.SourceOf(p)
			f, err := responsesFile(path, p, src)
			if err != nil {
				return nil, err
			}
			list = append(list, responses.ResponseFunctionCallOutputItemUnionParam{OfInputFile: &f})
		default:
			return nil, rejectPart(surfaceResponses, path, p, "function_call_output takes text, images and files")
		}
	}
	return list, nil
}

func appendResponsesAssistant(out responses.ResponseInputParam, mi int, v types.AssistantMessage) (responses.ResponseInputParam, error) {
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, responses.ResponseInputItemParamOfMessage(text.String(), responses.EasyInputMessageRoleAssistant))
			text.Reset()
		}
	}
	for pi, p := range v.Parts {
		switch bc := p.(type) {
		case types.TextPart:
			text.WriteString(bc.Text)
		case types.ToolCallPart:
			flush()
			args, _ := json.Marshal(bc.Arguments)
			out = append(out, responses.ResponseInputItemParamOfFunctionCall(string(args), bc.ID, bc.Name))
		case types.ThinkingPart:
			if item, ok := reasoningItem(bc); ok {
				flush()
				out = append(out, item)
			}
		case types.RefusalPart:
			flush()
			out = append(out, rawItem(map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "refusal", "refusal": bc.Text}},
			}))
		case types.AudioOutPart, types.ImageOutPart, types.VideoOutPart:
			return nil, rejectPart(surfaceResponses, topLevel(mi, pi), p, "assistant media cannot be sent back on this surface")
		}
		// Citations annotate text already sent, and server tool parts
		// from another provider have no input form here.
	}
	flush()
	return out, nil
}

// reasoningItem replays a reasoning item this API produced: one that
// carries encrypted content and is a summary or has no text. Reasoning from
// other providers, whose signatures this API cannot read, is not sent. The
// item ID is omitted: requests are stateless and the encrypted content is
// what the API reads.
func reasoningItem(t types.ThinkingPart) (responses.ResponseInputItemUnionParam, bool) {
	if t.Signature == "" || t.Redacted || (!t.Summary && t.Text != "") {
		return responses.ResponseInputItemUnionParam{}, false
	}
	summary := []map[string]any{}
	if t.Text != "" {
		summary = append(summary, map[string]any{"type": "summary_text", "text": t.Text})
	}
	return rawItem(map[string]any{"type": "reasoning", "summary": summary, "encrypted_content": t.Signature}), true
}

// rawItem is an input item sent exactly as v encodes. It carries shapes the
// SDK's typed params would add a required field to (an item ID), which a
// stateless request does not have.
func rawItem(v map[string]any) responses.ResponseInputItemUnionParam {
	b, _ := json.Marshal(v)
	r := param.Override[responses.ResponseReasoningItemParam](json.RawMessage(b))
	return responses.ResponseInputItemUnionParam{OfReasoning: &r}
}
