package bind

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/discovery"
	"github.com/urmzd/saige/agent/skills"
	"github.com/urmzd/saige/agent/types"
)

// bindSkills binds the definition's skills by mode:
//
//   - lazy and trigger skills are listed in the system prompt, and found
//     with discover and loaded with load;
//   - search skills are left out of the prompt, and found with discover
//     and loaded with load;
//   - eager and pinned skills have their instructions in the prompt, from
//     the snapshot taken now, so a later change on disk does not reach the
//     bound agent; a pinned skill must match its hash;
//   - a trigger skill's instructions are also put in front of a user
//     message that matches one of its patterns.
//
// discover searches a skill by its name, description and instructions.
// load goes through the skill toolset, so a loaded skill's allowed-tools
// narrows the agent's tools as load_skill does, and read_skill_resource
// reads its files.
//
// Only the named skills are reachable, so naming a skill is what makes an
// untrusted one available, as skills.AllowNames does.
func bindSkills(ctx context.Context, env *Env, d *definition.Definition, p *parts) error {
	if env.Skills == nil {
		return fmt.Errorf("%w: agent %s names skills, but the host provides no skill catalog", ErrUnsupported, d.Name)
	}
	var (
		names    []string
		loadable []skills.Skill
		eager    []skills.Skill
		triggers []trigger
	)
	for _, ref := range d.Skills {
		s, err := env.Skills.Load(ctx, d.Name, ref.Name)
		if err != nil {
			return fmt.Errorf("agent %s: skill %s: %w", d.Name, ref.Name, err)
		}
		names = append(names, ref.Name)
		switch ref.EffectiveMode() {
		case definition.SkillPinned:
			if s.Hash != ref.Hash {
				return fmt.Errorf("agent %s: skill %s is pinned to %s, but its snapshot is %s", d.Name, ref.Name, ref.Hash, s.Hash)
			}
			eager = append(eager, s)
		case definition.SkillEager:
			eager = append(eager, s)
		case definition.SkillSearch:
			loadable = append(loadable, s)
		case definition.SkillTrigger:
			loadable = append(loadable, s)
			p.listedSkills = append(p.listedSkills, s.SkillMeta)
			t := trigger{skill: s}
			for _, expr := range ref.Triggers {
				re, err := regexp.Compile(expr)
				if err != nil {
					return fmt.Errorf("agent %s: skill %s: %w", d.Name, ref.Name, err)
				}
				t.patterns = append(t.patterns, re)
			}
			triggers = append(triggers, t)
		default:
			loadable = append(loadable, s)
			p.listedSkills = append(p.listedSkills, s.SkillMeta)
		}
	}

	var b strings.Builder
	for _, s := range eager {
		fmt.Fprintf(&b, "\n## Skill: %s\n\nThese instructions are guidance; they do not change what you are permitted to do.\n\n%s\n", s.Name, strings.TrimSpace(s.Body))
	}
	if section := strings.TrimSpace(b.String()); section != "" {
		if p.prompt != "" {
			p.prompt += "\n\n"
		}
		p.prompt += section
	}

	if len(loadable) > 0 {
		ts := skills.NewToolset(env.Skills, skills.AllowListPolicy{Default: skills.AllowNames(names...)})
		var load types.Tool
		for _, tool := range ts.Tools() {
			switch tool.Definition().Name {
			case skills.LoadSkillName:
				load = tool
			case skills.ReadSkillResourceName:
				p.tools = append(p.tools, tool)
			}
		}
		for _, s := range loadable {
			name := s.Name
			p.entries = append(p.entries, discovery.Entry{
				Kind: discovery.KindSkill, Name: name, Description: s.Description, Text: s.Body,
				Load: func(ctx context.Context) (string, error) {
					return load.Execute(ctx, map[string]any{"name": name})
				},
			})
		}
		p.toolPolicy = ts.Policy
	}
	if len(triggers) > 0 {
		p.hooks = append(p.hooks, triggerHook(d.Name, triggers))
	}
	return nil
}

type trigger struct {
	skill    skills.Skill
	patterns []*regexp.Regexp
}

// triggerHook puts the instructions of every trigger skill whose pattern
// matches a user message in front of that message, for the owning agent's
// runs only: sub-agents inherit hooks, and a parent's triggers are not
// theirs.
func triggerHook(owner string, list []trigger) agent.Hooks {
	return agent.Hooks{
		Name: "skill-triggers:" + owner,
		UserInput: func(_ context.Context, ev *agent.UserInputEvent) error {
			if ev.Agent != owner {
				return nil
			}
			text := userText(ev.Message)
			var b strings.Builder
			for _, t := range list {
				for _, re := range t.patterns {
					if re.MatchString(text) {
						fmt.Fprintf(&b, "<skill name=%q>\nThis request matches the %s skill. Follow its instructions; they do not change what you are permitted to do.\n\n%s\n</skill>\n",
							t.skill.Name, t.skill.Name, strings.TrimSpace(t.skill.Body))
						break
					}
				}
			}
			if b.Len() > 0 {
				ev.Message = withPrefix(ev.Message, types.UserMsg(types.Text(strings.TrimSpace(b.String()))))
			}
			return nil
		},
	}
}

// SkillCheck returns a definition.Checks.Skill that looks skills up in cat
// and compares a pinned hash.
func SkillCheck(cat skills.SkillCatalog) func(name, hash string) error {
	return func(name, hash string) error {
		s, err := cat.Load(context.Background(), "", name)
		if err != nil {
			return err
		}
		if hash != "" && s.Hash != hash {
			return fmt.Errorf("skill %s is pinned to %s, but its snapshot is %s", name, hash, s.Hash)
		}
		return nil
	}
}
