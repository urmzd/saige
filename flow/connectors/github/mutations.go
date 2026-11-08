package github

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// MutationAPI supports explicit, collaborator-authorized changes to a PR head.
// CommitChanges must compare the expected head atomically with the branch write.
type MutationAPI interface {
	CanWrite(context.Context, Ref, string) (bool, error)
	CommitChanges(context.Context, Ref, string, string, ChangeRequest) (CommitResult, error)
}

// FileEdit replaces a complete UTF-8 text file, or deletes it. Paths are always
// repository-relative; branch and repository are bound by the triggering run.
type FileEdit struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}

// UnmarshalJSON distinguishes an intentional empty file from an omitted
// replacement, so a missing model argument cannot silently erase a file.
func (f *FileEdit) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
		Delete  bool    `json:"delete"`
	}
	if err := decodeArguments(raw, &wire); err != nil {
		return err
	}
	if !wire.Delete && wire.Content == nil {
		return fmt.Errorf("github: replacement edit needs content")
	}
	f.Path, f.Delete, f.Content = wire.Path, wire.Delete, ""
	if wire.Content != nil {
		f.Content = *wire.Content
	}
	return nil
}

// ChangeRequest is one atomic commit. All files succeed together or none do.
type ChangeRequest struct {
	Message string     `json:"message"`
	Files   []FileEdit `json:"files"`
}

// CommitResult records the actual commit produced by GitHub, not a model claim.
type CommitResult struct {
	SHA       string `json:"sha"`
	URL       string `json:"url"`
	Recovered bool   `json:"recovered,omitempty"`
}

func decodeArguments(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("github: arguments: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("github: expected one arguments object")
	}
	return nil
}

func validateChanges(args ChangeRequest) error {
	if strings.TrimSpace(args.Message) == "" || len(args.Message) > 200 || strings.ContainsAny(args.Message, "\r\n") {
		return fmt.Errorf("github: commit message must be one line of at most 200 bytes")
	}
	if len(args.Files) == 0 || len(args.Files) > 20 {
		return fmt.Errorf("github: commit needs 1 to 20 file edits")
	}
	total := 0
	seen := map[string]bool{}
	for _, file := range args.Files {
		if !utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, 0) {
			return fmt.Errorf("github: edits must be UTF-8 text")
		}
		if file.Path == "" || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path || file.Path == "." || file.Path == ".." || strings.HasPrefix(file.Path, "../") || strings.ContainsAny(file.Path, "\\\x00\r\n") {
			return fmt.Errorf("github: invalid repository file path %q", file.Path)
		}
		for _, segment := range strings.Split(file.Path, "/") {
			if strings.EqualFold(segment, ".git") {
				return fmt.Errorf("github: reserved file path")
			}
		}
		if seen[file.Path] {
			return fmt.Errorf("github: duplicate edit for %q", file.Path)
		}
		seen[file.Path] = true
		if file.Delete && file.Content != "" {
			return fmt.Errorf("github: deletion cannot include replacement content")
		}
		total += len(file.Content)
	}
	if total > 500000 {
		return fmt.Errorf("github: edit payload exceeds 500000 bytes")
	}
	return nil
}

func mutationMarker(run, expected string, args ChangeRequest) string {
	encoded, _ := json.Marshal(struct {
		Run, Head string
		Changes   ChangeRequest
	}{run, expected, args})
	sum := sha256.Sum256(encoded)
	return "Fiber-Change: " + hex.EncodeToString(sum[:])
}
