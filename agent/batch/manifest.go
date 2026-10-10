package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// ErrDuplicateID reports a batch with an empty or repeated custom ID.
var ErrDuplicateID = errors.New("batch: custom IDs must be unique and non-empty")

// CheckIDs rejects empty and repeated custom IDs.
func CheckIDs(reqs []types.BatchRequest) error {
	if len(reqs) == 0 {
		return fmt.Errorf("%w: batch has no requests", types.ErrInvalidModelConfig)
	}
	seen := make(map[string]bool, len(reqs))
	for i, r := range reqs {
		if r.CustomID == "" {
			return fmt.Errorf("%w: request %d has no custom ID", ErrDuplicateID, i)
		}
		if seen[r.CustomID] {
			return fmt.Errorf("%w: %q appears twice", ErrDuplicateID, r.CustomID)
		}
		seen[r.CustomID] = true
	}
	return nil
}

// Manifest returns a SHA-256 over everything a batch sends: each request's
// custom ID, messages (with content types), tools, schema and options, in
// order, plus the serving provider and model. Two submissions with the same
// manifest send the same work.
func Manifest(provider, model string, reqs []types.BatchRequest) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	if err := enc.Encode([]string{provider, model}); err != nil {
		return "", err
	}
	for i, r := range reqs {
		item := struct {
			ID       string                 `json:"id"`
			Messages []canonicalMessage     `json:"messages"`
			Tools    []types.ToolDef        `json:"tools,omitempty"`
			Schema   *types.ParameterSchema `json:"schema,omitempty"`
			Options  types.RequestOptions   `json:"options"`
		}{ID: r.CustomID, Tools: r.Tools, Schema: r.Schema, Options: r.Options}
		for _, m := range r.Messages {
			item.Messages = append(item.Messages, canonical(m))
		}
		if err := enc.Encode(item); err != nil {
			return "", fmt.Errorf("batch manifest: request %d: %w", i, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalMessage keeps each content block's Go type in the hash, so two
// blocks with the same fields but different kinds hash apart.
type canonicalMessage struct {
	Role    types.Role       `json:"role"`
	Content []canonicalBlock `json:"content"`
}

type canonicalBlock struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func canonical(m types.Message) canonicalMessage {
	out := canonicalMessage{Role: m.Role()}
	add := func(v any) { out.Content = append(out.Content, canonicalBlock{Type: fmt.Sprintf("%T", v), Value: v}) }
	switch v := m.(type) {
	case types.SystemMessage:
		for _, c := range v.Content {
			add(c)
		}
	case types.UserMessage:
		for _, c := range v.Content {
			add(c)
		}
	case types.AssistantMessage:
		for _, c := range v.Content {
			add(c)
		}
	}
	return out
}

// wireID is the vendor-safe custom ID of request i in a job: the job prefix
// and the index. It fits Anthropic's [A-Za-z0-9_-]{1,64} and every other
// vendor's alphabet, whatever the caller's IDs look like.
func wireID(prefix string, i int) string { return prefix + "-" + strconv.Itoa(i) }

// wireIndex parses a wire ID back to its index. ok is false for an ID that
// does not belong to the job.
func wireIndex(prefix, id string, n int) (int, bool) {
	rest, ok := strings.CutPrefix(id, prefix+"-")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(rest)
	if err != nil || i < 0 || i >= n || strconv.Itoa(i) != rest {
		return 0, false
	}
	return i, true
}

// wirePrefix derives a job's prefix from its ID and manifest, so two jobs
// never share request IDs and the prefix also serves as the vendor tag.
func wirePrefix(jobID, manifest string) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + manifest))
	return "sb" + hex.EncodeToString(sum[:])[:16]
}
