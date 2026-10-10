package workspace

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Names of the tools that work with artifacts by reference.
const (
	ReadArtifactToolName   = "read_artifact"
	SearchArtifactToolName = "search_artifact"
	CopyArtifactToolName   = "copy_artifact"
	CopyFileToolName       = "copy_file"
)

// MaxCopyFileBytes bounds the file copy_file stores.
const MaxCopyFileBytes = 64 << 20

// ArtifactTools returns read_artifact and search_artifact over ws. Like the
// scratch tools, each call uses the workspace attached to its context when
// there is one. They pull what a model needs from an artifact it was handed
// by reference, a page or a matching line at a time, so the whole content
// never has to enter its context.
func ArtifactTools(ws Workspace) []types.Tool {
	return []types.Tool{ReadArtifactTool(ws), SearchArtifactTool(ws)}
}

// ReadArtifactTool returns read_artifact.
func ReadArtifactTool(ws Workspace) types.Tool {
	return readTool(ws, ReadArtifactToolName,
		fmt.Sprintf("Read part of an artifact you were given by saige-artifact:// URI. Returns up to %d bytes from offset; page through with the offset the result names.", DefaultReadLimit),
		"uri", "The saige-artifact:// URI, or an artifact name.")
}

// SearchArtifactTool returns search_artifact.
func SearchArtifactTool(ws Workspace) types.Tool {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        SearchArtifactToolName,
			Description: "Find lines containing every word of the query in one artifact, or in every artifact you can read when uri is empty. Returns each match with its byte offset, for read_artifact.",
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{"query"},
				Properties: map[string]types.PropertyDef{
					"query": {Type: types.SchemaString, Description: "Words to find, case-insensitive."},
					"uri":   {Type: types.SchemaString, Description: "The saige-artifact:// URI to search. Empty searches everything."},
					"k":     {Type: types.SchemaInteger, Description: "Maximum matches. Default 10."},
				},
			},
			Capability: types.ToolCapabilityRead,
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			w, err := resolve(ctx, ws)
			if err != nil {
				return "", err
			}
			q, _ := args["query"].(string)
			k := int(intArg(args, "k", 10))
			var hits []Hit
			if uri, _ := args["uri"].(string); strings.TrimSpace(uri) != "" {
				ref, err := ParseRef(uri)
				if err != nil {
					return "", err
				}
				if ref, err = w.Stat(ctx, ref); err != nil {
					return "", err
				}
				hits, err = searchRefs(ctx, []Ref{ref}, func(r Ref) ([]byte, error) { return w.Read(ctx, r, 0, 0) }, q, k)
				if err != nil {
					return "", err
				}
			} else if hits, err = w.Search(ctx, q, k); err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "no matches", nil
			}
			var b strings.Builder
			for _, h := range hits {
				fmt.Fprintf(&b, "%s offset %d (line %d): %s\n", h.Ref.URI(), h.Offset, h.Line, h.Text)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// CopyArtifactTool returns copy_artifact, which stores an artifact under a
// new name. The content is copied inside the workspace and never passes
// through the model, so a model can file or rename a large result with a
// call of a few tokens.
func CopyArtifactTool(ws Workspace) types.Tool {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        CopyArtifactToolName,
			Description: "Store an artifact under a new name without reading it. Returns the copy's saige-artifact:// URI.",
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{"from", "name"},
				Properties: map[string]types.PropertyDef{
					"from": {Type: types.SchemaString, Description: "The saige-artifact:// URI or name to copy."},
					"name": {Type: types.SchemaString, Description: "The name to store the copy under."},
				},
			},
			Capability: types.ToolCapabilityWrite,
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			w, err := resolve(ctx, ws)
			if err != nil {
				return "", err
			}
			from, _ := args["from"].(string)
			src, err := ParseRef(from)
			if err != nil {
				return "", err
			}
			name, _ := args["name"].(string)
			ref, err := Copy(ctx, w, src, w, name)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("copied to %s (%d bytes) as %s", ref.Name, ref.Size, ref.URI()), nil
		},
	}
}

// CopyFileTool returns copy_file, which stores a file under root in the
// workspace and returns a reference to it with a short preview. The model
// gets a URI to read from instead of the file's content, so a large file
// costs its context only the parts it reads. Paths are resolved inside root
// with os.Root, so a path cannot escape it through .. or a symbolic link.
func CopyFileTool(ws Workspace, root string) types.Tool {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        CopyFileToolName,
			Description: "Store a file in the workspace without reading it into the conversation. Returns its saige-artifact:// URI and a short preview; read more with read_artifact.",
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{"path"},
				Properties: map[string]types.PropertyDef{
					"path": {Type: types.SchemaString, Description: "File path relative to the allowed root."},
					"name": {Type: types.SchemaString, Description: "Name to store it under. Default: files/<path>."},
				},
			},
			Capability: types.ToolCapabilityWrite,
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			w, err := resolve(ctx, ws)
			if err != nil {
				return "", err
			}
			path, _ := args["path"].(string)
			path = filepath.Clean(strings.TrimSpace(path))
			data, err := readInRoot(root, path)
			if err != nil {
				return "", err
			}
			name, _ := args["name"].(string)
			if strings.TrimSpace(name) == "" {
				name = "files/" + filepath.ToSlash(path)
			}
			ref, err := NewReference(ctx, w, name, data, ReferenceOptions{Meta: map[string]string{"path": filepath.ToSlash(path)}})
			if err != nil {
				return "", err
			}
			return ref.String(), nil
		},
	}
}

// readInRoot reads path inside root, refusing files over MaxCopyFileBytes.
func readInRoot(root, path string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	f, err := r.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxCopyFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCopyFileBytes {
		return nil, fmt.Errorf("workspace: %s is larger than %d bytes", path, MaxCopyFileBytes)
	}
	return data, nil
}
