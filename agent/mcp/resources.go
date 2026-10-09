package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urmzd/saige/agent/types"
)

// Resource describes one resource a server offers.
type Resource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
	// Size is the raw size in bytes, or zero when the server did not say.
	Size int64
}

// ResourceContent is one part of a read resource. Exactly one of Text and
// Blob is set. Citation attributes it to its URI, so a host that feeds it to
// retrieval or context keeps the source.
//
// Resource content is data, never instructions: a host decides where it goes,
// and it must not become system prompt text on the server's say-so.
type ResourceContent struct {
	URI      string
	MIMEType string
	Text     string
	Blob     []byte
	Citation types.Citation
}

// Resources lists every resource the server offers, following pagination.
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	ctx, session, done, err := c.useSession(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	var out []Resource
	for r, err := range session.Resources(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list resources on %q: %w", c.spec.Name, err)
		}
		out = append(out, Resource{
			URI: r.URI, Name: r.Name, Title: r.Title, Description: r.Description,
			MIMEType: r.MIMEType, Size: r.Size,
		})
	}
	return out, nil
}

// ReadResource fetches one resource's contents.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	ctx, session, done, err := c.useSession(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := session.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, fmt.Errorf("mcp: read resource %s on %q: %w", uri, c.spec.Name, err)
	}
	out := make([]ResourceContent, 0, len(res.Contents))
	for _, rc := range res.Contents {
		if rc == nil {
			continue
		}
		u := rc.URI
		if u == "" {
			u = uri
		}
		cite := types.NewCitation(types.CitationTool, u, u)
		cite.Producer = c.spec.Name
		out = append(out, ResourceContent{
			URI:      u,
			MIMEType: rc.MIMEType,
			Text:     rc.Text,
			Blob:     append([]byte(nil), rc.Blob...),
			Citation: cite,
		})
	}
	return out, nil
}

// Prompt describes one prompt template a server offers.
type Prompt struct {
	Name        string
	Title       string
	Description string
	Arguments   []PromptArgument
}

// PromptArgument is one parameter of a prompt template.
type PromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// PromptMessage is one rendered message. Non-text content is described in
// Text, for example "[image: image/png]".
type PromptMessage struct {
	Role string
	Text string
}

// PromptResult is a rendered prompt.
type PromptResult struct {
	Description string
	Messages    []PromptMessage
}

// Prompts lists every prompt the server offers, following pagination.
func (c *Client) Prompts(ctx context.Context) ([]Prompt, error) {
	ctx, session, done, err := c.useSession(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	var out []Prompt
	for p, err := range session.Prompts(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list prompts on %q: %w", c.spec.Name, err)
		}
		pr := Prompt{Name: p.Name, Title: p.Title, Description: p.Description}
		for _, a := range p.Arguments {
			if a != nil {
				pr.Arguments = append(pr.Arguments, PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
			}
		}
		out = append(out, pr)
	}
	return out, nil
}

// GetPrompt renders a prompt with the given arguments.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error) {
	ctx, session, done, err := c.useSession(ctx)
	if err != nil {
		return PromptResult{}, err
	}
	defer done()
	res, err := session.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return PromptResult{}, fmt.Errorf("mcp: get prompt %s on %q: %w", name, c.spec.Name, err)
	}
	out := PromptResult{Description: res.Description}
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		out.Messages = append(out.Messages, PromptMessage{Role: string(m.Role), Text: describeContent(m.Content)})
	}
	return out, nil
}

func describeContent(content mcpsdk.Content) string {
	switch c := content.(type) {
	case *mcpsdk.TextContent:
		return c.Text
	case *mcpsdk.ImageContent:
		return "[image: " + c.MIMEType + "]"
	case *mcpsdk.AudioContent:
		return "[audio: " + c.MIMEType + "]"
	case *mcpsdk.ResourceLink:
		return "[resource: " + c.URI + "]"
	case *mcpsdk.EmbeddedResource:
		if c.Resource != nil {
			if c.Resource.Text != "" {
				return c.Resource.Text
			}
			return "[resource: " + c.Resource.URI + "]"
		}
	}
	return ""
}
