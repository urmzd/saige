package native_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/intake"
	"github.com/urmzd/saige/flow/attachment/resolve"
	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/turn/native"
)

const filesTenant = "acme"

// stored puts content in the blob store the way an upload does and returns the
// reference a tag would carry.
func stored(t *testing.T, store blob.Store, tenant, name, content string) attachment.Attachment {
	t.Helper()
	att, err := intake.Store(context.Background(), store, tenant, strings.NewReader(content), intake.Meta{Name: name})
	if err != nil {
		t.Fatalf("store %s: %v", name, err)
	}
	return att
}

// A file reaches the model on the message it arrived with: after the tools,
// the system prompt and the whole history, and after the request text. Nothing
// a later turn could share a cached prefix with comes after it.
func TestAnAttachedFileReachesTheModelOnTheTurnsOwnMessage(t *testing.T) {
	t.Parallel()
	store := blob.NewMemory()
	log := stored(t, store, filesTenant, "deploy.log", "step three: migrate failed with exit code 7\n")

	history := []native.Message{
		{Role: native.RoleUser, Text: "the deploy failed"},
		{Role: native.RoleAssistant, Text: "send me the log"},
	}
	turn := native.Turn{Text: "here it is", Tenant: filesTenant, Attachments: []attachment.Attachment{log}}

	with, _ := threadRun(t, history, turn, 0, native.WithAttachments(resolve.New(store)))
	without, _ := threadRun(t, history, native.Turn{Text: "here it is"}, 0)

	got, plain := with[0].Messages, without[0].Messages
	if len(got) != len(plain) {
		t.Fatalf("the file changed the message count: %d with, %d without", len(got), len(plain))
	}
	last := len(got) - 1
	if a, b := wireMessages(t, got[:last]), wireMessages(t, plain[:last]); string(a) != string(b) {
		t.Error("an attachment changed messages ahead of the one it arrived with")
	}
	body := string(wireMessages(t, got[last:]))
	if !strings.Contains(body, "migrate failed with exit code 7") {
		t.Errorf("the file's content is not in the turn's message:\n%s", body)
	}
	if !strings.Contains(body, "deploy.log") {
		t.Errorf("the file is not named in the turn's message:\n%s", body)
	}
	if at, text := strings.Index(body, "migrate failed"), strings.Index(body, "here it is"); text < 0 || at < text {
		t.Errorf("the file must come after the request text; text at %d, file at %d", text, at)
	}
}

// A turn with files and no way to read them is refused. Answering it would be
// answering a different question.
func TestATurnWithFilesAndNoResolverIsRefused(t *testing.T) {
	t.Parallel()
	store := blob.NewMemory()
	log := stored(t, store, filesTenant, "deploy.log", "x\n")

	r, err := native.New(offlineRevision(1, "You are docs-bot."))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Run(context.Background(), native.Turn{Text: "look", Tenant: filesTenant, Attachments: []attachment.Attachment{log}}, nil)
	if !errors.Is(err, native.ErrInvalid) {
		t.Fatalf("Run: got %v, want ErrInvalid", err)
	}
}

// The tenant on the turn scopes the read. A reference to another tenant's file
// produces a note that a file could not be read, and none of its content.
func TestAnotherTenantsFileNeverReachesTheModel(t *testing.T) {
	t.Parallel()
	store := blob.NewMemory()
	theirs := stored(t, store, "globex", "secret.txt", "the globex launch code is 0000\n")

	turn := native.Turn{Text: "what does this say?", Tenant: filesTenant, Attachments: []attachment.Attachment{theirs}}
	model, _ := threadRun(t, nil, turn, 0, native.WithAttachments(resolve.New(store)))

	body := string(wireMessages(t, model[0].Messages))
	if strings.Contains(body, "launch code") {
		t.Fatal("another tenant's file content reached the model")
	}
	if !strings.Contains(body, "secret.txt") {
		t.Errorf("the unreadable file was dropped silently instead of being noted:\n%s", body)
	}
}
