package harness

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
)

// ManifestFile is the conventional file name of a suite [Manifest].
const ManifestFile = "saige.eval.json"

// ManifestVersion is the only manifest schema version this package reads.
const ManifestVersion = 1

// ManifestProviders are the provider names a [Manifest] subject may use.
// An empty provider means [OpenAICompatible].
var ManifestProviders = []string{OpenAICompatible, providerOpenAI, providerAnthropic, providerGoogle, providerOllama}

// Provider names of the saige adapters a manifest can select.
const (
	providerOpenAI    = "openai"
	providerAnthropic = "anthropic"
	providerGoogle    = "google"
	providerOllama    = "ollama"
)

// BuiltinFlows lists the flow names a [Manifest] may select.
var BuiltinFlows = []string{baseFlowName, statelessFlowName}

// Metrics lists the metric names the runner scores each turn with, the
// names a [Manifest] assertion may gate on.
var Metrics = []string{MetricTurnSucceeded, MetricLatencyMs, MetricInputTokens, MetricOutputTokens, MetricOutputBytes}

// Manifest declares one eval suite: the corpus, the model under test, the
// flows, how to run, and the gates that decide pass or fail. It is decoded
// strictly: an unknown key is an error, so a misspelled setting cannot be
// ignored silently. Relative paths resolve against the manifest's directory.
//
// The manifest never holds a secret. A subject names the environment
// variable that holds its key, and the key is read at run time.
type Manifest struct {
	Version     int    `json:"version"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Corpus is the corpus directory (see [LoadCorpus]).
	Corpus string `json:"corpus"`
	// IDPrefix and Count select scripts as [FilterScripts] does.
	IDPrefix string `json:"id_prefix,omitempty"`
	Count    int    `json:"count,omitempty"`
	// Flows names built-in flows in order; empty means base then stateless.
	Flows   []string         `json:"flows,omitempty"`
	Subject ManifestSubject  `json:"subject,omitzero"`
	Policy  ManifestPolicy   `json:"policy,omitzero"`
	Assert  []eval.Assertion `json:"assert,omitempty"`
	// Store is the results store directory; empty records nothing.
	Store string `json:"store,omitempty"`

	dir string
}

// ManifestSubject is the model under test.
type ManifestSubject struct {
	// Provider is one of [ManifestProviders]; empty means
	// [OpenAICompatible].
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// BaseURL is the API base for the openai-compatible and openai
	// providers.
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the key of the
	// openai-compatible provider. The other providers read their own
	// variable (for example ANTHROPIC_API_KEY).
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// ManifestPolicy controls execution.
type ManifestPolicy struct {
	// Concurrency is how many scripts run at once; zero means one.
	Concurrency int `json:"concurrency,omitempty"`
	// ContinueOnError keeps going after a script fails; nil means true.
	ContinueOnError *bool `json:"continue_on_error,omitempty"`
}

// LoadManifest reads and validates a manifest file. Issues carry the file
// path. The manifest is nil when the file does not decode; otherwise it is
// returned even with issues, and HasErrors(issues) says whether it is
// usable. The error is non-nil only when the file cannot be read.
func LoadManifest(path string) (*Manifest, []Issue, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, nil, err
	}
	m, issues := ParseManifest(data)
	if m != nil {
		m.dir = filepath.Dir(path)
	}
	return m, withFile(path, issues), nil
}

// ParseManifest decodes and validates manifest JSON. Relative paths resolve
// against the current directory; [LoadManifest] resolves them against the
// file's directory instead.
func ParseManifest(data []byte) (*Manifest, []Issue) {
	var m Manifest
	issues, ok := decodeStrict(data, &m)
	if !ok {
		return nil, issues
	}
	return &m, append(issues, m.Validate()...)
}

// Dir is the directory relative paths resolve against.
func (m *Manifest) Dir() string { return m.dir }

// Resolve returns path resolved against the manifest's directory, or path
// unchanged when it is empty or absolute.
func (m *Manifest) Resolve(path string) string {
	if path == "" || filepath.IsAbs(path) || m.dir == "" {
		return path
	}
	return filepath.Join(m.dir, path)
}

// ProviderName returns the subject's provider, defaulting to
// [OpenAICompatible].
func (m *Manifest) ProviderName() string {
	if m.Subject.Provider == "" {
		return OpenAICompatible
	}
	return m.Subject.Provider
}

// ContinueOnError resolves the policy default of true.
func (m *Manifest) ContinueOnError() bool {
	return m.Policy.ContinueOnError == nil || *m.Policy.ContinueOnError
}

// FlowNames returns the selected flows, defaulting to base then stateless.
func (m *Manifest) FlowNames() []string {
	if len(m.Flows) == 0 {
		return slices.Clone(BuiltinFlows)
	}
	return slices.Clone(m.Flows)
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// catalogProviders are the providers whose model names the catalog
// declares; other providers serve arbitrary model names.
var catalogProviders = []string{providerOpenAI, providerAnthropic, providerGoogle}

// Validate checks the manifest without touching the network or the
// environment, so it runs offline and in CI. A model the catalog does not
// declare is a warning here; a run decides whether to refuse it.
func (m *Manifest) Validate() []Issue {
	var issues []Issue
	switch {
	case m.Version == 0:
		issues = append(issues, errorIssue("/version", "version is required; set it to %d", ManifestVersion))
	case m.Version != ManifestVersion:
		issues = append(issues, errorIssue("/version", "unsupported version %d; this saige reads version %d", m.Version, ManifestVersion))
	}
	if m.Corpus == "" {
		issues = append(issues, errorIssue("/corpus", "corpus is required: the directory of scripts to run"))
	}
	if m.Count < 0 {
		issues = append(issues, errorIssue("/count", "count must be zero (all) or positive"))
	}
	seen := map[string]bool{}
	for i, name := range m.Flows {
		pointer := fmt.Sprintf("/flows/%d", i)
		switch {
		case !slices.Contains(BuiltinFlows, name):
			issues = append(issues, errorIssue(pointer, "unknown flow %q (known flows: %v)", name, BuiltinFlows))
		case seen[name]:
			issues = append(issues, errorIssue(pointer, "flow %q is listed twice", name))
		}
		seen[name] = true
	}
	issues = append(issues, m.validateSubject()...)
	if m.Policy.Concurrency < 0 {
		issues = append(issues, errorIssue("/policy/concurrency", "concurrency must be zero (one at a time) or positive"))
	}
	return append(issues, ValidateAssertions(m.Assert)...)
}

func (m *Manifest) validateSubject() []Issue {
	var issues []Issue
	s := m.Subject
	provider := m.ProviderName()
	if !slices.Contains(ManifestProviders, provider) {
		issues = append(issues, errorIssue("/subject/provider", "unknown provider %q (known providers: %v)", s.Provider, ManifestProviders))
		return issues
	}
	if s.BaseURL != "" {
		if provider != OpenAICompatible && provider != providerOpenAI {
			issues = append(issues, errorIssue("/subject/base_url", "base_url applies to the %s and openai providers, not %s", OpenAICompatible, provider))
		} else if u, err := url.Parse(s.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			issues = append(issues, errorIssue("/subject/base_url", "base_url must be an http or https URL"))
		}
	}
	if s.APIKeyEnv != "" {
		switch {
		case !envNameRE.MatchString(s.APIKeyEnv):
			issues = append(issues, errorIssue("/subject/api_key_env", "api_key_env names the environment variable that holds the key, not the key itself"))
		case provider != OpenAICompatible:
			issues = append(issues, errorIssue("/subject/api_key_env", "api_key_env applies to the %s provider; %s reads its own key variable", OpenAICompatible, provider))
		}
	}
	if s.Model != "" && slices.Contains(catalogProviders, provider) {
		if _, ok := catalog.Describe(types.ProviderName(provider), s.Model); !ok {
			issues = append(issues, warningIssue("/subject/model", "model %q is not in the %s catalog; check the name, or allow unknown models when running", s.Model, provider))
		}
	}
	return issues
}

// ValidateAssertions checks gates against the metrics the runner scores,
// addressing issues as /assert/<index>/<field>.
func ValidateAssertions(assertions []eval.Assertion) []Issue {
	var issues []Issue
	for i, a := range assertions {
		issues = append(issues, validateAssertion(fmt.Sprintf("/assert/%d", i), a)...)
	}
	return issues
}

// validateAssertion checks one gate against the metrics the runner scores.
func validateAssertion(pointer string, a eval.Assertion) []Issue {
	var issues []Issue
	if !slices.Contains(Metrics, a.Metric) {
		msg := fmt.Sprintf("unknown metric %q (known metrics: %v)", a.Metric, Metrics)
		if near := closest(a.Metric, Metrics); near != "" {
			msg = fmt.Sprintf("unknown metric %q (did you mean %q?)", a.Metric, near)
		}
		issues = append(issues, errorIssue(pointer+"/metric", "%s", msg))
	}
	if !slices.Contains([]eval.Op{eval.GTE, eval.LTE, eval.EQ}, a.Op) {
		issues = append(issues, errorIssue(pointer+"/op", "op must be one of >=, <=, =="))
	}
	if a.Scope != "" && a.Scope != eval.EveryCase && a.Scope != eval.OnAggregate {
		issues = append(issues, errorIssue(pointer+"/scope", "scope must be %q or %q", eval.EveryCase, eval.OnAggregate))
	}
	if a.MinPassRate < 0 || a.MinPassRate > 1 {
		issues = append(issues, errorIssue(pointer+"/min_pass_rate", "min_pass_rate must be in [0, 1]"))
	}
	if a.MinPassRate > 0 && a.Scope == eval.OnAggregate {
		issues = append(issues, warningIssue(pointer+"/min_pass_rate", "min_pass_rate has no effect on an aggregate assertion"))
	}
	return issues
}
