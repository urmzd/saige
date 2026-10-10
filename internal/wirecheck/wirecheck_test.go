package wirecheck_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	p0 "github.com/urmzd/saige/agent"
	p1 "github.com/urmzd/saige/agent/batch"
	p2 "github.com/urmzd/saige/agent/definition"
	p3 "github.com/urmzd/saige/agent/definition/bind"
	p4 "github.com/urmzd/saige/agent/durable/duraturo"
	p5 "github.com/urmzd/saige/agent/durable/local"
	p6 "github.com/urmzd/saige/agent/mcp"
	p7 "github.com/urmzd/saige/agent/memory"
	p8 "github.com/urmzd/saige/agent/memory/pgstore"
	p9 "github.com/urmzd/saige/agent/pgstore"
	p10 "github.com/urmzd/saige/agent/privacy"
	p11 "github.com/urmzd/saige/agent/provider"
	p12 "github.com/urmzd/saige/agent/provider/cache"
	p13 "github.com/urmzd/saige/agent/provider/catalog"
	p14 "github.com/urmzd/saige/agent/provider/ollama"
	p15 "github.com/urmzd/saige/agent/provider/preset"
	p16 "github.com/urmzd/saige/agent/provider/router"
	p17 "github.com/urmzd/saige/agent/provider/split"
	p18 "github.com/urmzd/saige/agent/selector/rank"
	p19 "github.com/urmzd/saige/agent/skills"
	p20 "github.com/urmzd/saige/agent/toolcache"
	p21 "github.com/urmzd/saige/agent/tree"
	types "github.com/urmzd/saige/agent/types"
	p23 "github.com/urmzd/saige/agent/workspace"
	p24 "github.com/urmzd/saige/eval"
	p25 "github.com/urmzd/saige/eval/harness"
	p26 "github.com/urmzd/saige/eval/online"
	p27 "github.com/urmzd/saige/eval/pgstore"
	p28 "github.com/urmzd/saige/eval/store"
	p29 "github.com/urmzd/saige/postgres"
	p30 "github.com/urmzd/saige/rag/bm25retriever"
	p31 "github.com/urmzd/saige/rag/eval"
	p32 "github.com/urmzd/saige/rag/extractor"
	p33 "github.com/urmzd/saige/rag/knowledge/types"
	p34 "github.com/urmzd/saige/rag/source"
	p35 "github.com/urmzd/saige/rag/types"
	p36 "github.com/urmzd/saige/tools"
	p37 "github.com/urmzd/saige/tools/exec"
	p38 "github.com/urmzd/saige/tools/fetch"
	p39 "github.com/urmzd/saige/tools/fs"
)

// sentinel is one exported Err* variable of the module.
type sentinel struct {
	pkg, name string
	err       error
}

// sentinels lists every exported sentinel error outside cmd, examples and
// internal packages. TestEverySentinelIsListed keeps it complete.
func sentinels() []sentinel {
	return []sentinel{
		{"agent", "ErrAncestorDelegation", p0.ErrAncestorDelegation},
		{"agent", "ErrHandoffLimitExceeded", p0.ErrHandoffLimitExceeded},
		{"agent", "ErrHookTimeout", p0.ErrHookTimeout},
		{"agent", "ErrInvalidSubmission", p0.ErrInvalidSubmission},
		{"agent", "ErrMarkerResolved", p0.ErrMarkerResolved},
		{"agent", "ErrNoJSON", p0.ErrNoJSON},
		{"agent", "ErrNothingToContinue", p0.ErrNothingToContinue},
		{"agent", "ErrRepeatedToolCalls", p0.ErrRepeatedToolCalls},
		{"agent", "ErrRunActive", p0.ErrRunActive},
		{"agent", "ErrRunFinished", p0.ErrRunFinished},
		{"agent", "ErrSchemaInvalid", p0.ErrSchemaInvalid},
		{"agent", "ErrSpawnUnsupported", p0.ErrSpawnUnsupported},
		{"agent", "ErrSubmitUnsupported", p0.ErrSubmitUnsupported},
		{"agent", "ErrToolErrorLimit", p0.ErrToolErrorLimit},
		{"agent", "ErrUnknownHandle", p0.ErrUnknownHandle},
		{"agent", "ErrUnknownHandoffTarget", p0.ErrUnknownHandoffTarget},
		{"agent", "ErrUnknownMarker", p0.ErrUnknownMarker},
		{"agent/batch", "ErrDuplicateID", p1.ErrDuplicateID},
		{"agent/batch", "ErrIndeterminate", p1.ErrIndeterminate},
		{"agent/batch", "ErrJobConflict", p1.ErrJobConflict},
		{"agent/batch", "ErrJobFailed", p1.ErrJobFailed},
		{"agent/batch", "ErrJobNotFound", p1.ErrJobNotFound},
		{"agent/batch", "ErrManifestMismatch", p1.ErrManifestMismatch},
		{"agent/batch", "ErrNotEnded", p1.ErrNotEnded},
		{"agent/batch", "ErrNotSubmitted", p1.ErrNotSubmitted},
		{"agent/batch", "ErrSubmitIncomplete", p1.ErrSubmitIncomplete},
		{"agent/definition", "ErrInvalid", p2.ErrInvalid},
		{"agent/definition", "ErrNotFound", p2.ErrNotFound},
		{"agent/definition/bind", "ErrUnsupported", p3.ErrUnsupported},
		{"agent/durable/duraturo", "ErrClosed", p4.ErrClosed},
		{"agent/durable/duraturo", "ErrConflict", p4.ErrConflict},
		{"agent/durable/duraturo", "ErrFailed", p4.ErrFailed},
		{"agent/durable/duraturo", "ErrIndeterminate", p4.ErrIndeterminate},
		{"agent/durable/local", "ErrBusy", p5.ErrBusy},
		{"agent/durable/local", "ErrClosed", p5.ErrClosed},
		{"agent/durable/local", "ErrConflict", p5.ErrConflict},
		{"agent/durable/local", "ErrIndeterminate", p5.ErrIndeterminate},
		{"agent/durable/local", "ErrNoNotifier", p5.ErrNoNotifier},
		{"agent/durable/local", "ErrSignal", p5.ErrSignal},
		{"agent/mcp", "ErrBlockedAddress", p6.ErrBlockedAddress},
		{"agent/memory", "ErrKindNotAllowed", p7.ErrKindNotAllowed},
		{"agent/memory", "ErrNoScope", p7.ErrNoScope},
		{"agent/memory", "ErrNotFound", p7.ErrNotFound},
		{"agent/memory", "ErrReadOnly", p7.ErrReadOnly},
		{"agent/memory", "ErrSensitive", p7.ErrSensitive},
		{"agent/memory", "ErrUnsupported", p7.ErrUnsupported},
		{"agent/memory/pgstore", "ErrForeignConversation", p8.ErrForeignConversation},
		{"agent/pgstore", "ErrConversationMismatch", p9.ErrConversationMismatch},
		{"agent/privacy", "ErrAudioOutRefused", p10.ErrAudioOutRefused},
		{"agent/privacy", "ErrMediaRefused", p10.ErrMediaRefused},
		{"agent/provider", "ErrUnknownProvider", p11.ErrUnknownProvider},
		{"agent/provider/cache", "ErrResponseCodec", p12.ErrResponseCodec},
		{"agent/provider/catalog", "ErrInvalidCatalog", p13.ErrInvalidCatalog},
		{"agent/provider/ollama", "ErrStreamIdle", p14.ErrStreamIdle},
		{"agent/provider/preset", "ErrNoLocalModel", p15.ErrNoLocalModel},
		{"agent/provider/router", "ErrNoEligibleProfile", p16.ErrNoEligibleProfile},
		{"agent/provider/router", "ErrSessionBusy", p16.ErrSessionBusy},
		{"agent/provider/router", "ErrUnknownProfile", p16.ErrUnknownProfile},
		{"agent/provider/split", "ErrUnknownVariant", p17.ErrUnknownVariant},
		{"agent/selector/rank", "ErrEmptyQuery", p18.ErrEmptyQuery},
		{"agent/skills", "ErrBoundsExceeded", p19.ErrBoundsExceeded},
		{"agent/skills", "ErrFrontmatter", p19.ErrFrontmatter},
		{"agent/skills", "ErrIntegrity", p19.ErrIntegrity},
		{"agent/skills", "ErrInvalidSkill", p19.ErrInvalidSkill},
		{"agent/skills", "ErrOutsideSkillRoot", p19.ErrOutsideSkillRoot},
		{"agent/skills", "ErrResourceNotFound", p19.ErrResourceNotFound},
		{"agent/skills", "ErrResourceNotText", p19.ErrResourceNotText},
		{"agent/skills", "ErrResourceTooLarge", p19.ErrResourceTooLarge},
		{"agent/skills", "ErrSkillNotFound", p19.ErrSkillNotFound},
		{"agent/skills", "ErrSkillNotReachable", p19.ErrSkillNotReachable},
		{"agent/toolcache", "ErrEntryCodec", p20.ErrEntryCodec},
		{"agent/tree", "ErrBranchNotFound", p21.ErrBranchNotFound},
		{"agent/tree", "ErrCheckpointNotFound", p21.ErrCheckpointNotFound},
		{"agent/tree", "ErrInvalidBranchPoint", p21.ErrInvalidBranchPoint},
		{"agent/tree", "ErrInvalidRoot", p21.ErrInvalidRoot},
		{"agent/tree", "ErrMessageFormatVersion", p21.ErrMessageFormatVersion},
		{"agent/tree", "ErrNodeArchived", p21.ErrNodeArchived},
		{"agent/tree", "ErrNodeIsLeaf", p21.ErrNodeIsLeaf},
		{"agent/tree", "ErrNodeNotFound", p21.ErrNodeNotFound},
		{"agent/tree", "ErrRootImmutable", p21.ErrRootImmutable},
		{"agent/tree", "ErrStoreWrite", p21.ErrStoreWrite},
		{"agent/tree", "ErrTreeFormatVersion", p21.ErrTreeFormatVersion},
		{"agent/tree", "ErrWALCommit", p21.ErrWALCommit},
		{"agent/types", "ErrAuth", types.ErrAuth},
		{"agent/types", "ErrBatchAmbiguous", types.ErrBatchAmbiguous},
		{"agent/types", "ErrBatchNotFound", types.ErrBatchNotFound},
		{"agent/types", "ErrBatchRequest", types.ErrBatchRequest},
		{"agent/types", "ErrBudgetAdmission", types.ErrBudgetAdmission},
		{"agent/types", "ErrBudgetBusy", types.ErrBudgetBusy},
		{"agent/types", "ErrBudgetExceeded", types.ErrBudgetExceeded},
		{"agent/types", "ErrContentFiltered", types.ErrContentFiltered},
		{"agent/types", "ErrContextLength", types.ErrContextLength},
		{"agent/types", "ErrGuardrailTripped", types.ErrGuardrailTripped},
		{"agent/types", "ErrHookAborted", types.ErrHookAborted},
		{"agent/types", "ErrInterruptExpired", types.ErrInterruptExpired},
		{"agent/types", "ErrInterruptNotFound", types.ErrInterruptNotFound},
		{"agent/types", "ErrInvalidChannel", types.ErrInvalidChannel},
		{"agent/types", "ErrInvalidConfig", types.ErrInvalidConfig},
		{"agent/types", "ErrInvalidGrant", types.ErrInvalidGrant},
		{"agent/types", "ErrInvalidModelConfig", types.ErrInvalidModelConfig},
		{"agent/types", "ErrInvalidRequest", types.ErrInvalidRequest},
		{"agent/types", "ErrInvalidTarget", types.ErrInvalidTarget},
		{"agent/types", "ErrInvalidToolArguments", types.ErrInvalidToolArguments},
		{"agent/types", "ErrMaxIterations", types.ErrMaxIterations},
		{"agent/types", "ErrMediaUnavailable", types.ErrMediaUnavailable},
		{"agent/types", "ErrModalityUnsupported", types.ErrModalityUnsupported},
		{"agent/types", "ErrNoInterruptRouter", types.ErrNoInterruptRouter},
		{"agent/types", "ErrNotifierClosed", types.ErrNotifierClosed},
		{"agent/types", "ErrOptionsUnsupported", types.ErrOptionsUnsupported},
		{"agent/types", "ErrPartRole", types.ErrPartRole},
		{"agent/types", "ErrProviderFailed", types.ErrProviderFailed},
		{"agent/types", "ErrRateLimited", types.ErrRateLimited},
		{"agent/types", "ErrReservationActive", types.ErrReservationActive},
		{"agent/types", "ErrResolverNotFound", types.ErrResolverNotFound},
		{"agent/types", "ErrResponseTruncated", types.ErrResponseTruncated},
		{"agent/types", "ErrSchemaMismatch", types.ErrSchemaMismatch},
		{"agent/types", "ErrSchemaUnsupported", types.ErrSchemaUnsupported},
		{"agent/types", "ErrSplitToolCall", types.ErrSplitToolCall},
		{"agent/types", "ErrStreamCanceled", types.ErrStreamCanceled},
		{"agent/types", "ErrSuspended", types.ErrSuspended},
		{"agent/types", "ErrToolExists", types.ErrToolExists},
		{"agent/types", "ErrToolNotFound", types.ErrToolNotFound},
		{"agent/types", "ErrToolQuotaExceeded", types.ErrToolQuotaExceeded},
		{"agent/types", "ErrUnavailable", types.ErrUnavailable},
		{"agent/types", "ErrUnknownPartKind", types.ErrUnknownPartKind},
		{"agent/types", "ErrUnknownReservation", types.ErrUnknownReservation},
		{"agent/types", "ErrUnknownTarget", types.ErrUnknownTarget},
		{"agent/types", "ErrUnknownWireKind", types.ErrUnknownWireKind},
		{"agent/types", "ErrUnpriced", types.ErrUnpriced},
		{"agent/types", "ErrUnsupportedMediaType", types.ErrUnsupportedMediaType},
		{"agent/types", "ErrUntrustedLocator", types.ErrUntrustedLocator},
		{"agent/types", "ErrVersionConflict", types.ErrVersionConflict},
		{"agent/types", "ErrWireInlineTooLarge", types.ErrWireInlineTooLarge},
		{"agent/types", "ErrWireUnrepresentable", types.ErrWireUnrepresentable},
		{"agent/types", "ErrWireVersion", types.ErrWireVersion},
		{"agent/workspace", "ErrInvalidRef", p23.ErrInvalidRef},
		{"agent/workspace", "ErrNotFound", p23.ErrNotFound},
		{"agent/workspace", "ErrReadOnly", p23.ErrReadOnly},
		{"eval", "ErrInfra", p24.ErrInfra},
		{"eval", "ErrInlineInput", p24.ErrInlineInput},
		{"eval", "ErrJudgeMedia", p24.ErrJudgeMedia},
		{"eval", "ErrNoJudgeScore", p24.ErrNoJudgeScore},
		{"eval", "ErrNotApplicable", p24.ErrNotApplicable},
		{"eval", "ErrOutputNotJSON", p24.ErrOutputNotJSON},
		{"eval", "ErrUnknownScorer", p24.ErrUnknownScorer},
		{"eval", "ErrUnlocatedMedia", p24.ErrUnlocatedMedia},
		{"eval/harness", "ErrAssertionsFailed", p25.ErrAssertionsFailed},
		{"eval/harness", "ErrInconclusive", p25.ErrInconclusive},
		{"eval/online", "ErrNotFinished", p26.ErrNotFinished},
		{"eval/pgstore", "ErrSchemaMissing", p27.ErrSchemaMissing},
		{"eval/store", "ErrDuplicateUnit", p28.ErrDuplicateUnit},
		{"eval/store", "ErrInvalidID", p28.ErrInvalidID},
		{"eval/store", "ErrNotFound", p28.ErrNotFound},
		{"eval/store", "ErrRunExists", p28.ErrRunExists},
		{"postgres", "ErrEmbeddingDimMismatch", p29.ErrEmbeddingDimMismatch},
		{"postgres", "ErrExtensionUnavailable", p29.ErrExtensionUnavailable},
		{"postgres", "ErrUnsupportedServer", p29.ErrUnsupportedServer},
		{"rag/bm25retriever", "ErrRebuildUnsupported", p30.ErrRebuildUnsupported},
		{"rag/eval", "ErrUnknownRelevanceKey", p31.ErrUnknownRelevanceKey},
		{"rag/extractor", "ErrNoText", p32.ErrNoText},
		{"rag/knowledge/types", "ErrNoEmbedder", p33.ErrNoEmbedder},
		{"rag/knowledge/types", "ErrNoExtractor", p33.ErrNoExtractor},
		{"rag/knowledge/types", "ErrNodeNotFound", p33.ErrNodeNotFound},
		{"rag/knowledge/types", "ErrPartialEpisode", p33.ErrPartialEpisode},
		{"rag/knowledge/types", "ErrStoreNotReady", p33.ErrStoreNotReady},
		{"rag/source", "ErrTooLarge", p34.ErrTooLarge},
		{"rag/types", "ErrDocumentNotFound", p35.ErrDocumentNotFound},
		{"rag/types", "ErrDuplicateDocument", p35.ErrDuplicateDocument},
		{"rag/types", "ErrEmbeddingShape", p35.ErrEmbeddingShape},
		{"rag/types", "ErrInvalidKeywordQuery", p35.ErrInvalidKeywordQuery},
		{"rag/types", "ErrNoExtractor", p35.ErrNoExtractor},
		{"rag/types", "ErrNoRetriever", p35.ErrNoRetriever},
		{"rag/types", "ErrNoStore", p35.ErrNoStore},
		{"rag/types", "ErrPartialIngest", p35.ErrPartialIngest},
		{"rag/types", "ErrPartialSearch", p35.ErrPartialSearch},
		{"rag/types", "ErrScopeMismatch", p35.ErrScopeMismatch},
		{"rag/types", "ErrSyncUnsupported", p35.ErrSyncUnsupported},
		{"rag/types", "ErrUnsupportedMIMEType", p35.ErrUnsupportedMIMEType},
		{"rag/types", "ErrVariantNotFound", p35.ErrVariantNotFound},
		{"rag/types", "ErrVariantPart", p35.ErrVariantPart},
		{"tools", "ErrNoRoot", p36.ErrNoRoot},
		{"tools/exec", "ErrCommandDenied", p37.ErrCommandDenied},
		{"tools/exec", "ErrDockerUnavailable", p37.ErrDockerUnavailable},
		{"tools/exec", "ErrMountNotShared", p37.ErrMountNotShared},
		{"tools/exec", "ErrNetworkIsolationUnavailable", p37.ErrNetworkIsolationUnavailable},
		{"tools/exec", "ErrNetworkUnenforced", p37.ErrNetworkUnenforced},
		{"tools/exec", "ErrNoLanguages", p37.ErrNoLanguages},
		{"tools/fetch", "ErrBlockedAddress", p38.ErrBlockedAddress},
		{"tools/fs", "ErrNoRoot", p39.ErrNoRoot},
	}
}

// Every exported sentinel has a wire code, survives EncodeError,
// JSON and DecodeError, and still matches with errors.Is, bare and wrapped.
func TestEverySentinelRoundTrips(t *testing.T) {
	registered := map[error]string{}
	for code, err := range types.WireSentinels() {
		if prev, dup := registered[err]; dup {
			t.Errorf("%v has two wire codes, %q and %q", err, prev, code)
		}
		registered[err] = code
	}
	for _, s := range sentinels() {
		t.Run(s.pkg+"."+s.name, func(t *testing.T) {
			if _, ok := registered[s.err]; !ok {
				t.Fatalf("%s.%s has no wire code (types.RegisterWireSentinel)", s.pkg, s.name)
			}
			for _, err := range []error{s.err, fmt.Errorf("context: %w", s.err)} {
				raw, mErr := json.Marshal(types.EncodeError(err))
				if mErr != nil {
					t.Fatal(mErr)
				}
				var enc types.EncodedError
				if uErr := json.Unmarshal(raw, &enc); uErr != nil {
					t.Fatal(uErr)
				}
				got := types.DecodeError(&enc)
				if !errors.Is(got, s.err) {
					t.Fatalf("decoded %q does not match %s.%s", got, s.pkg, s.name)
				}
				if got.Error() != err.Error() {
					t.Fatalf("message %q, want %q", got.Error(), err.Error())
				}
				if types.KindOf(got) != types.KindOf(err) {
					t.Fatalf("kind %v, want %v", types.KindOf(got), types.KindOf(err))
				}
			}
		})
	}
}

// The list above names every exported Err* variable in the module, so a
// new sentinel cannot skip registration.
func TestEverySentinelIsListed(t *testing.T) {
	listed := map[string]bool{}
	for _, s := range sentinels() {
		listed[s.pkg+"."+s.name] = true
	}
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch n := info.Name(); {
			case p == root:
			case n == "testdata", n == "examples", n == "cmd", n == "internal", strings.HasPrefix(n, "."):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil || f.Name.Name == "main" {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "Err") || !n.IsExported() || i >= len(vs.Values) {
						continue
					}
					if _, alias := vs.Values[i].(*ast.SelectorExpr); alias {
						continue // another package's sentinel under a second name
					}
					if !listed[filepath.ToSlash(rel)+"."+n.Name] {
						t.Errorf("%s.%s is not listed in sentinels()", filepath.ToSlash(rel), n.Name)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
