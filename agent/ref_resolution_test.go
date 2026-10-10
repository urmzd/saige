package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

func pngOffering() types.Offering {
	return types.Offering{ID: "acme/vision@acme", Model: types.ModelInfo{Vendor: "acme", Prefix: "vision", Known: true},
		Endpoint: types.EndpointInfo{Name: "acme"},
		Modalities: types.Modalities{In: map[types.Modality]types.ModalityLimit{
			types.ModalityImage: {Media: []types.MediaType{types.MediaPNG}},
		}}}
}

// storedImage is an image as a stored conversation reads back: a
// saige-artifact reference and digest, no bytes.
func storedImage(data []byte) types.ImagePart {
	src := types.Artifact(types.ArtifactScheme+workspace.Digest(data), types.MediaPNG)
	src.Size = int64(len(data))
	return types.Image(src)
}

// TestReloadedRefsAreSentAsBytes checks that media stored as a workspace
// reference is sent to the provider with its bytes again, in a user
// message and inside a tool result.
func TestReloadedRefsAreSentAsBytes(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemory()
	data := []byte("png bytes")
	if _, err := ws.Put(ctx, "media/"+workspace.Digest(data), data, map[string]string{"media_type": string(types.MediaPNG)}); err != nil {
		t.Fatal(err)
	}
	p := &offeringProvider{name: "acme", offering: pngOffering()}
	a := NewAgent(AgentConfig{Provider: p}, WithWorkspace(ws))
	history := []types.Message{
		types.UserMsg(types.Text("look"), storedImage(data)),
		types.AssistantMsg(types.ToolCallPart{ID: "c1", Name: "chart", Arguments: map[string]any{}}),
		types.UserToolResults(types.ToolOK("c1", storedImage(data))),
		types.UserMsg(types.Text("again")),
	}
	if _, err := runConverting(t, a, history...); err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, m := range p.lastSent() {
		mapMedia(m, func(part types.Part) types.Part {
			if src, ok := types.SourceOf(part); ok {
				seen++
				if !bytes.Equal(src.Inline, data) || src.Ref == "" {
					t.Errorf("sent source = %+v, want the bytes and the ref", src)
				}
			}
			return part
		})
	}
	if seen != 2 {
		t.Fatalf("media sent = %d, want 2", seen)
	}
}

// TestUnresolvableRefIsRejectedNotDropped checks that a reference no store
// holds marks the part unavailable, so the plan rejects the call with
// ErrMediaUnavailable instead of sending the turn without it.
func TestUnresolvableRefIsRejectedNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []AgentOption
	}{
		{name: "no workspace"},
		{name: "workspace without it", opts: []AgentOption{WithWorkspace(workspace.NewMemory())}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &offeringProvider{name: "acme", offering: pngOffering()}
			a := NewAgent(AgentConfig{Provider: p}, tc.opts...)
			_, err := runConverting(t, a, types.UserMsg(types.Text("look"), storedImage([]byte("gone"))))
			if !errors.Is(err, types.ErrMediaUnavailable) {
				t.Fatalf("err = %v, want ErrMediaUnavailable", err)
			}
			if len(p.sent) != 0 {
				t.Error("the request reached the provider without its media")
			}
		})
	}
}

// TestResolveRefsFallsBackToTheResolver checks that a host's
// saige-artifact resolver serves a ref the workspace does not hold, that a
// digest mismatch is unavailable, and that a part with another locator is
// left for it.
func TestResolveRefsFallsBackToTheResolver(t *testing.T) {
	data := []byte("uploaded")
	resolver := types.ResolverFunc(func(_ context.Context, uri string) (types.ResolvedFile, error) {
		if uri == types.ArtifactScheme+workspace.Digest(data) {
			return types.ResolvedFile{Data: data, MediaType: types.MediaPNG}, nil
		}
		return types.ResolvedFile{Data: []byte("other")}, nil
	})
	a := NewAgent(AgentConfig{Provider: &capturingProvider{response: "ok"},
		Resolvers: map[string]types.Resolver{workspace.URIScheme: resolver}}, WithWorkspace(workspace.NewMemory()))

	mismatched := storedImage([]byte("expected"))
	withURL := storedImage([]byte("missing"))
	withURL.Source.URI = "https://example.com/a.png"
	out := a.resolveSources(context.Background(), []types.Message{types.UserMsg(storedImage(data), mismatched, withURL)})
	parts := out[0].(types.UserMessage).Parts
	if src, _ := types.SourceOf(parts[0]); !bytes.Equal(src.Inline, data) || src.Unresolved != "" {
		t.Errorf("resolved = %+v", src)
	}
	if src, _ := types.SourceOf(parts[1]); len(src.Inline) != 0 || !strings.Contains(src.Unresolved, "do not match") {
		t.Errorf("mismatched = %+v, want unavailable", src)
	}
	if src, _ := types.SourceOf(parts[2]); len(src.Inline) != 0 || src.Unresolved != "" || src.URI == "" {
		t.Errorf("with a URL = %+v, want it left for the URL", src)
	}
}

// TestExternalizeStoresForeignRefs checks that bytes carrying a ref the
// workspace does not hold (a host upload) are stored, so the record can be
// restored from the workspace, and that a held ref is not written again.
func TestExternalizeStoresForeignRefs(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemory()
	a := NewAgent(AgentConfig{Provider: &capturingProvider{response: "ok"}}, WithWorkspace(ws))
	data := []byte("upload")
	src := types.Bytes(types.MediaPNG, data)
	src.Ref = types.ArtifactScheme + src.Digest
	a.externalize(ctx, types.UserMsg(types.Image(src)))
	ref, _ := workspace.ParseRef(src.Ref)
	if got, err := ws.Read(ctx, ref, 0, 0); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("workspace holds %q, %v", got, err)
	}
	refs, _ := ws.List(ctx)
	a.externalize(ctx, types.UserMsg(types.Image(src)))
	if again, _ := ws.List(ctx); len(again) != len(refs) {
		t.Fatalf("refs = %d after a second commit, want %d", len(again), len(refs))
	}
}

// TestStoreReloadSendsStoredMediaBytes runs a turn with an image through a
// store and a workspace, reloads the tree from the store alone, and checks
// that the next turn sends the image's bytes again.
func TestStoreReloadSendsStoredMediaBytes(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	ws := workspace.NewMemory()
	data := []byte("png bytes")
	first := &offeringProvider{name: "acme", offering: pngOffering()}
	a := NewAgent(AgentConfig{Provider: first, SystemPrompt: "s"}, WithStore(store), WithWorkspace(ws))
	if _, err := runConverting(t, a, types.UserMsg(types.Text("look"), types.Image(types.Bytes(types.MediaPNG, data)))); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadTreeFromStore(ctx, codecStore{store}, a.Tree().Root().ID, a.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	second := &offeringProvider{name: "acme", offering: pngOffering()}
	b := NewAgent(AgentConfig{Provider: second, SystemPrompt: "s"}, WithTree(restored), WithWorkspace(ws))
	if _, err := runConverting(t, b, types.UserMsg(types.Text("and now?"))); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, m := range second.lastSent() {
		for _, img := range types.Each[types.ImagePart](m) {
			got = img.Source.Inline
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("image bytes sent after reload = %q, want %q", got, data)
	}
}

// codecStore reads nodes back through the message codec, as a store that
// serializes them does, so their media keep locators but no bytes.
type codecStore struct{ *memstore.Store }

func (s codecStore) LoadTree(ctx context.Context, root types.NodeID) ([]*types.Node, map[types.BranchID]types.NodeID, error) {
	nodes, branches, err := s.Store.LoadTree(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	for _, n := range nodes {
		if n.Message == nil {
			continue
		}
		raw, err := tree.MarshalMessage(n.Message)
		if err != nil {
			return nil, nil, err
		}
		if n.Message, err = tree.UnmarshalMessage(n.Message.Role(), raw); err != nil {
			return nil, nil, err
		}
	}
	return nodes, branches, nil
}
