package definition

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Harness groups a definition may name.
var harnessGroups = []string{"read", "write", "exec", "web"}

// Capability classes and grant scopes an approval may name.
var (
	capabilityClasses = []string{
		string(types.ToolCapabilityRead), string(types.ToolCapabilityWrite),
		string(types.ToolCapabilityDestructive), string(types.ToolCapabilityUnknown),
	}
	grantScopes = []string{
		string(types.GrantOnce), string(types.GrantArgs), string(types.GrantTool), string(types.GrantSession),
	}
	memoryKinds       = []string{"semantic", "episodic", "procedural"}
	compactStrategies = []string{
		string(types.CompactNone), string(types.CompactSlidingWindow), string(types.CompactSummarize),
		string(types.CompactClearToolResults), string(types.CompactKeepRecent), string(types.CompactSummary),
		string(types.CompactRelevantPlusSummary), string(types.CompactChain),
	}
)

var (
	skillHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// patternLabel is the label rule of privacy.NewRegexDetector.
	patternLabel = regexp.MustCompile(`^[A-Z0-9_]+$`)
)

// GrantRank orders grant scopes from narrowest to widest, for comparing an
// approval's grant with ApprovalSpec.Grant.
func GrantRank(scope types.GrantScope) int {
	switch scope {
	case types.GrantOnce, "":
		return 0
	case types.GrantArgs:
		return 1
	case types.GrantTool:
		return 2
	case types.GrantSession:
		return 3
	}
	return 4
}

// Validate checks what a definition can prove on its own: the version, the
// name, every closed vocabulary, rule and pattern syntax, and values that
// contradict each other. References to other definitions, skills and
// presets are checked by Registry. Errors are a *ValidationError.
func (d *Definition) Validate() error {
	var found issues
	d.check(&found)
	return found.asError(d.Location())
}

func (d *Definition) check(found *issues) {
	switch {
	case d.APIVersion == "":
		found.add("apiVersion", 0, CodeMissing, "apiVersion is required; use %q", APIVersion)
	case d.APIVersion != APIVersion:
		found.add("apiVersion", 0, CodeVersion, "unsupported apiVersion %q; this reader understands %q", d.APIVersion, APIVersion)
	}
	if err := checkName(d.Name); err != nil {
		found.add("name", 0, codeFor(d.Name), "%v", err)
	}
	if d.Version == "" {
		found.add("version", 0, CodeMissing, "version is required, such as 1.0.0")
	} else if _, err := ParseVersion(d.Version); err != nil {
		found.add("version", 0, CodeBadValue, "%v", err)
	}
	if d.Model != nil {
		checkModel(d.Model, found)
	}
	if d.Dials != nil {
		if err := d.Dials.Validate(); err != nil {
			found.add("dials", 0, CodeBadValue, "%v", err)
		}
	}
	if d.Tools != nil {
		checkTools(d.Tools, found)
	}
	checkSkills(d.Skills, found)
	if d.Memory != nil {
		checkMemory(d.Memory, found)
	}
	checkSubagents(d.Name, d.Subagents, found)
	if d.Approval != nil {
		checkApproval(d.Approval, found)
	}
	if d.Compaction != nil {
		checkCompaction("compaction", d.Compaction.Strategy, d.Compaction.Chain != nil, found)
		for i, step := range d.Compaction.Chain {
			checkCompaction(fmt.Sprintf("compaction.chain[%d]", i), step.Strategy, step.Chain != nil, found)
		}
	}
	if d.Guardrails != nil {
		for i, g := range d.Guardrails.Input {
			checkGuardrail(fmt.Sprintf("guardrails.input[%d]", i), g, true, found)
		}
		for i, g := range d.Guardrails.Output {
			checkGuardrail(fmt.Sprintf("guardrails.output[%d]", i), g, false, found)
		}
	}
	if d.Limits != nil {
		checkLimits(d.Limits, found)
	}
}

func codeFor(v string) string {
	if v == "" {
		return CodeMissing
	}
	return CodeBadValue
}

func oneOf(found *issues, path, v string, allowed []string) bool {
	if slices.Contains(allowed, v) {
		return true
	}
	msg := fmt.Sprintf("%q is not one of %s", v, strings.Join(allowed, ", "))
	if s := suggest(v, allowed); s != "" {
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	found.add(path, 0, CodeBadValue, "%s", msg)
	return false
}

func checkProviderModel(found *issues, path, ref string) {
	prov, model, ok := strings.Cut(ref, "/")
	if !ok || prov == "" || model == "" {
		found.add(path, 0, CodeBadValue, "%q must be provider/model, such as anthropic/claude-haiku-5-5", ref)
	}
}

func checkModel(m *ModelRef, found *issues) {
	switch {
	case strings.TrimSpace(m.Use) == "":
		found.add("model.use", 0, CodeMissing, "model needs a preset name or provider/model")
	case !m.IsPreset():
		checkProviderModel(found, "model.use", m.Use)
	}
	for i, f := range m.Fallback {
		checkProviderModel(found, fmt.Sprintf("model.fallback[%d]", i), f)
	}
}

func checkUnique(found *issues, path string, list []string) {
	seen := map[string]bool{}
	for i, v := range list {
		if seen[v] {
			found.add(fmt.Sprintf("%s[%d]", path, i), 0, CodeBadValue, "%q is listed twice", v)
		}
		seen[v] = true
	}
}

func checkTools(t *ToolsSpec, found *issues) {
	for i, g := range t.Harness {
		oneOf(found, fmt.Sprintf("tools.harness[%d]", i), g, harnessGroups)
	}
	checkUnique(found, "tools.harness", t.Harness)
	var servers []string
	for i, m := range t.MCP {
		if m.Server == "" {
			found.add(fmt.Sprintf("tools.mcp[%d].server", i), 0, CodeMissing, "server is required")
		}
		servers = append(servers, m.Server)
	}
	checkUnique(found, "tools.mcp", servers)
	for i, name := range t.Registry {
		if strings.TrimSpace(name) == "" {
			found.add(fmt.Sprintf("tools.registry[%d]", i), 0, CodeMissing, "tool name is required")
		}
	}
	checkUnique(found, "tools.registry", t.Registry)
}

func checkSkills(list []SkillRef, found *issues) {
	var names []string
	for i, s := range list {
		path := fmt.Sprintf("skills[%d]", i)
		names = append(names, s.Name)
		if !skillName.MatchString(s.Name) {
			found.add(path+".name", 0, codeFor(s.Name), "skill name %q must be lowercase letters and digits separated by hyphens", s.Name)
		}
		var modes []string
		for _, m := range SkillModes() {
			modes = append(modes, string(m))
		}
		if s.Mode != "" && !oneOf(found, path+".mode", string(s.Mode), modes) {
			continue
		}
		mode := s.EffectiveMode()
		switch {
		case mode == SkillTrigger && len(s.Triggers) == 0:
			found.add(path+".triggers", 0, CodeMissing, "a trigger skill needs at least one trigger pattern")
		case mode != SkillTrigger && len(s.Triggers) > 0:
			found.add(path+".triggers", 0, CodeBadValue, "triggers apply only to mode trigger")
		}
		for j, expr := range s.Triggers {
			if _, err := regexp.Compile(expr); err != nil {
				found.add(fmt.Sprintf("%s.triggers[%d]", path, j), 0, CodeBadValue, "invalid pattern: %v", err)
			}
		}
		switch {
		case mode == SkillPinned && s.Hash == "":
			found.add(path+".hash", 0, CodeMissing, "a pinned skill needs the hash of its snapshot (saige agent show prints it)")
		case mode != SkillPinned && s.Hash != "":
			found.add(path+".hash", 0, CodeBadValue, "hash applies only to mode pinned")
		case s.Hash != "" && !skillHash.MatchString(s.Hash):
			found.add(path+".hash", 0, CodeBadValue, "hash must be sha256:<64 hex digits>")
		}
	}
	checkUnique(found, "skills", names)
}

func checkMemory(m *MemorySpec, found *issues) {
	if m.Store == "" {
		found.add("memory.store", 0, CodeMissing, "store names a memory store the host provides")
	}
	if m.Recall != "" {
		oneOf(found, "memory.recall", m.Recall, []string{RecallTool, RecallInject, RecallSelect, RecallOff})
	}
	for i, k := range m.Write {
		oneOf(found, fmt.Sprintf("memory.write[%d]", i), k, memoryKinds)
	}
	if m.ReadOnly && len(m.Write) > 0 {
		found.add("memory.write", 0, CodeBadValue, "a read_only memory cannot list kinds to write")
	}
	if m.Budget < 0 {
		found.add("memory.budget", 0, CodeBadValue, "must not be negative")
	}
	if m.Retention < 0 {
		found.add("memory.retention", 0, CodeBadValue, "must not be negative")
	}
	if m.Namespace != "" {
		for part := range strings.SplitSeq(m.Namespace, "/") {
			if part == "" || part == "." || part == ".." {
				found.add("memory.namespace", 0, CodeBadValue, "invalid namespace %q", m.Namespace)
				break
			}
		}
	}
}

func checkSubagents(self string, list []SubagentRef, found *issues) {
	seen := map[string]bool{}
	for i, s := range list {
		path := fmt.Sprintf("subagents[%d]", i)
		ref, err := ParseRef(s.Ref)
		if err != nil {
			found.add(path+".ref", 0, codeFor(s.Ref), "%v", err)
			continue
		}
		if ref.Name == self && self != "" {
			found.add(path+".ref", 0, CodeCycle, "an agent cannot be its own sub-agent")
		}
		if seen[ref.Name] {
			found.add(path+".ref", 0, CodeBadValue, "sub-agent %q is listed twice", ref.Name)
		}
		seen[ref.Name] = true
		mode := s.EffectiveMode()
		if s.Mode != "" {
			oneOf(found, path+".mode", s.Mode, []string{SubagentDelegate, SubagentSpawn, SubagentHandoff})
		}
		if b := s.Budget; b != nil {
			if b.MaxIterations < 0 || b.Timeout < 0 || b.MaxCost < 0 || b.MaxTokens < 0 || b.MaxRequests < 0 {
				found.add(path+".budget", 0, CodeBadValue, "budget values must not be negative")
			}
			if mode == SubagentHandoff && (b.Timeout != 0 || b.MaxCost != 0 || b.MaxTokens != 0 || b.MaxRequests != 0) {
				found.add(path+".budget", 0, CodeBadValue,
					"a handoff member shares the entry agent's run and budget; only max_iterations applies")
			}
		}
	}
}

func checkApproval(a *ApprovalSpec, found *issues) {
	decisions := []string{DecisionAllow, DecisionAsk, DecisionDeny}
	for class, decision := range a.Capabilities {
		path := joinPath("approval.capabilities", class)
		oneOf(found, path, class, capabilityClasses)
		oneOf(found, path, decision, decisions)
	}
	for _, list := range []struct {
		name  string
		rules []string
	}{{"allow", a.Allow}, {"ask", a.Ask}, {"deny", a.Deny}} {
		for i, r := range list.rules {
			if _, err := ParseRule(r); err != nil {
				found.add(fmt.Sprintf("approval.%s[%d]", list.name, i), 0, CodeBadValue, "%v", err)
			}
		}
	}
	if a.Grant != "" {
		oneOf(found, "approval.grant", a.Grant, grantScopes)
	}
	if a.DenyAfter < 0 {
		found.add("approval.deny_after", 0, CodeBadValue, "must not be negative")
	}
	if a.RampAfter < 0 {
		found.add("approval.ramp_after", 0, CodeBadValue, "must not be negative")
	}
	if a.HideDenied && a.DenyAfter == 0 {
		found.add("approval.hide_denied", 0, CodeBadValue, "hide_denied needs deny_after")
	}
}

func checkCompaction(path, strategy string, hasChain bool, found *issues) {
	if !oneOf(found, path+".strategy", strategy, compactStrategies) {
		return
	}
	if (strategy == string(types.CompactChain)) != hasChain {
		found.add(path+".chain", 0, CodeBadValue, "chain is required with strategy chain and only valid there")
	}
}

func checkGuardrail(path string, g GuardrailSpec, input bool, found *issues) {
	if !oneOf(found, path+".name", g.Name, []string{GuardrailPII, GuardrailRegex, GuardrailMaxLength, GuardrailClassifier}) {
		return
	}
	unused := func(field string, set bool) {
		if set {
			found.add(path+"."+field, 0, CodeBadValue, "%s does not apply to %s", field, g.Name)
		}
	}
	unused("redact", g.Redact && g.Name != GuardrailPII && g.Name != GuardrailRegex)
	unused("max", g.Max != 0 && g.Name != GuardrailMaxLength)
	unused("patterns", len(g.Patterns) > 0 && g.Name != GuardrailRegex)
	unused("policy", g.Policy != "" && g.Name != GuardrailClassifier)
	if g.Parallel && !input {
		found.add(path+".parallel", 0, CodeBadValue, "only an input guardrail can run in parallel")
	}
	if g.Parallel && g.Redact {
		found.add(path+".parallel", 0, CodeBadValue, "a parallel guardrail cannot redact: the model already has the text")
	}
	switch g.Name {
	case GuardrailMaxLength:
		if g.Max <= 0 {
			found.add(path+".max", 0, CodeMissing, "max_length needs a positive max")
		}
	case GuardrailRegex:
		if len(g.Patterns) == 0 {
			found.add(path+".patterns", 0, CodeMissing, "regex needs at least one pattern")
		}
		for i, p := range g.Patterns {
			pp := fmt.Sprintf("%s.patterns[%d]", path, i)
			if !patternLabel.MatchString(p.Label) {
				found.add(pp+".label", 0, codeFor(p.Label), "label %q must be upper-case letters, digits or underscores, such as TICKET_ID", p.Label)
			}
			if _, err := regexp.Compile(p.Expr); err != nil || p.Expr == "" {
				found.add(pp+".expr", 0, CodeBadValue, "invalid pattern %q", p.Expr)
			}
		}
	case GuardrailClassifier:
		if strings.TrimSpace(g.Policy) == "" {
			found.add(path+".policy", 0, CodeMissing, "classifier needs a policy")
		}
	}
}

func checkLimits(l *LimitsSpec, found *issues) {
	if l.MaxIterations < 0 {
		found.add("limits.max_iterations", 0, CodeBadValue, "must not be negative")
	}
	if l.LLMTimeout < 0 || l.ToolTimeout < 0 {
		found.add("limits", 0, CodeBadValue, "timeouts must not be negative")
	}
	b := l.Budget
	if b == nil {
		return
	}
	if b.MaxCost < 0 || b.MaxTokens < 0 || b.MaxRequests < 0 {
		found.add("limits.budget", 0, CodeBadValue, "budget values must not be negative")
	}
	if b.WarnAt < 0 || b.WarnAt > 1 {
		found.add("limits.budget.warn_at", 0, CodeBadValue, "warn_at is a fraction between 0 and 1")
	}
	if b.OnExceed != "" {
		oneOf(found, "limits.budget.on_exceed", b.OnExceed, []string{OnExceedStop, OnExceedAsk})
	}
	if b.MaxCost == 0 && b.MaxTokens == 0 && b.MaxRequests == 0 {
		found.add("limits.budget", 0, CodeMissing, "a budget needs max_cost, max_tokens or max_requests")
	}
}
