package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/types"
)

// stopRefusal is the stop reason of a response the model declined.
const stopRefusal = string(anthropic.StopReasonRefusal)

// rawCitation is the wire form of a text block citation. One struct reads
// every location type: char_location, page_location,
// content_block_location (documents), web_search_result_location and
// search_result_location.
type rawCitation struct {
	Type              string `json:"type"`
	CitedText         string `json:"cited_text"`
	DocumentIndex     *int64 `json:"document_index"`
	DocumentTitle     string `json:"document_title"`
	FileID            string `json:"file_id"`
	StartCharIndex    *int64 `json:"start_char_index"`
	EndCharIndex      *int64 `json:"end_char_index"`
	StartPageNumber   *int64 `json:"start_page_number"`
	EndPageNumber     *int64 `json:"end_page_number"`
	StartBlockIndex   *int64 `json:"start_block_index"`
	EndBlockIndex     *int64 `json:"end_block_index"`
	URL               string `json:"url"`
	Title             string `json:"title"`
	EncryptedIndex    string `json:"encrypted_index"`
	SearchResultIndex *int64 `json:"search_result_index"`
	Source            string `json:"source"`
}

// citationFrom converts one citation's JSON. A document location keeps the
// document index and its char, page or block range in Meta under the API's
// field names, plus "location" for the type.
func citationFrom(raw string) (types.Citation, bool) {
	var rc rawCitation
	if json.Unmarshal([]byte(raw), &rc) != nil || rc.Type == "" {
		return types.Citation{}, false
	}
	var c types.Citation
	switch rc.Type {
	case "web_search_result_location":
		c = types.NewCitation(types.CitationWeb, rc.URL, rc.Title)
	case "search_result_location":
		c = types.NewCitation(types.CitationRetrieval, rc.Source, rc.Title)
	default:
		c = types.NewCitation(types.CitationDocument, "", rc.DocumentTitle)
	}
	c.Quote, c.Producer = rc.CitedText, providerName
	c.Meta = map[string]any{"location": rc.Type}
	for k, v := range map[string]*int64{
		"document_index": rc.DocumentIndex, "start_char_index": rc.StartCharIndex, "end_char_index": rc.EndCharIndex,
		"start_page_number": rc.StartPageNumber, "end_page_number": rc.EndPageNumber,
		"start_block_index": rc.StartBlockIndex, "end_block_index": rc.EndBlockIndex,
		"search_result_index": rc.SearchResultIndex,
	} {
		if v != nil {
			c.Meta[k] = int(*v)
		}
	}
	if rc.FileID != "" {
		c.Meta["file_id"] = rc.FileID
	}
	if rc.EncryptedIndex != "" {
		c.Meta["encrypted_index"] = rc.EncryptedIndex
	}
	return c, true
}

// anchored returns the citation as a part anchored to the whole text part
// at position pos, which is n bytes long: Anthropic splits text into blocks
// so that a block's citations cover all of it.
func anchored(c types.Citation, pos, n int) types.CitationPart {
	return types.CitationPart{Citation: c, Anchor: &types.Anchor{PartIndex: pos, Start: 0, End: n}}
}

// refusalPart records a refusal stop with its category and explanation.
func refusalPart(d anthropic.RefusalStopDetails) types.RefusalPart {
	return types.RefusalPart{Text: d.Explanation, Category: string(d.Category)}
}

// serverToolResult decodes a server tool result block (web_search_tool_result,
// code_execution_tool_result, and the bash and text-editor variants) into a
// result part with a readable projection. The raw content is kept as the
// provider-native payload, which is what a later turn replays, and files a
// code execution run wrote become file parts with their Files API IDs.
func (a *Adapter) serverToolResult(raw string, kind types.ServerToolKind) types.ServerToolResultPart {
	var blk struct {
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal([]byte(raw), &blk)
	p := types.ServerToolResultPart{CallID: blk.ToolUseID, ToolKind: kind, Result: blk.Content}

	type item struct {
		Type       string `json:"type"`
		URL        string `json:"url"`
		Title      string `json:"title"`
		ErrorCode  string `json:"error_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		ReturnCode *int   `json:"return_code"`
		Content    []struct {
			FileID string `json:"file_id"`
		} `json:"content"`
	}
	var list []item
	if json.Unmarshal(blk.Content, &list) != nil {
		var one item
		if json.Unmarshal(blk.Content, &one) == nil {
			list = []item{one}
		}
	}
	var lines []string
	for _, it := range list {
		switch {
		case strings.HasSuffix(it.Type, "_error"):
			p.IsError = true
			lines = append(lines, "error: "+it.ErrorCode)
		case it.URL != "":
			lines = append(lines, strings.TrimSpace(it.Title+" "+it.URL))
		default:
			if it.ReturnCode != nil && *it.ReturnCode != 0 {
				p.IsError = true
			}
			if it.Stdout != "" {
				lines = append(lines, it.Stdout)
			}
			if it.Stderr != "" {
				lines = append(lines, it.Stderr)
			}
		}
		for _, f := range it.Content {
			if f.FileID != "" {
				p.Outputs = append(p.Outputs, types.File(types.VendorFileID(providerName, a.endpoint, f.FileID, "")))
			}
		}
	}
	p.Text = strings.Join(lines, "\n")
	return p
}

// serverResultKind names the kind of a server tool result block, from the
// call it answers or else from the block type.
func serverResultKind(blockType, callID string, calls map[string]types.ServerToolKind) types.ServerToolKind {
	if kind, ok := calls[callID]; ok {
		return kind
	}
	return serverToolKind(strings.TrimSuffix(blockType, "_tool_result"))
}

// isServerToolResult reports a server tool result block type.
func isServerToolResult(blockType string) bool {
	return strings.HasSuffix(blockType, "_tool_result")
}
