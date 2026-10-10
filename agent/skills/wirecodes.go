package skills

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("skills.bounds_exceeded", ErrBoundsExceeded)
	agenttypes.RegisterWireSentinel("skills.frontmatter", ErrFrontmatter)
	agenttypes.RegisterWireSentinel("skills.integrity", ErrIntegrity)
	agenttypes.RegisterWireSentinel("skills.invalid_skill", ErrInvalidSkill)
	agenttypes.RegisterWireSentinel("skills.outside_skill_root", ErrOutsideSkillRoot)
	agenttypes.RegisterWireSentinel("skills.resource_not_found", ErrResourceNotFound)
	agenttypes.RegisterWireSentinel("skills.resource_not_text", ErrResourceNotText)
	agenttypes.RegisterWireSentinel("skills.resource_too_large", ErrResourceTooLarge)
	agenttypes.RegisterWireSentinel("skills.skill_not_found", ErrSkillNotFound)
	agenttypes.RegisterWireSentinel("skills.skill_not_reachable", ErrSkillNotReachable)
}
