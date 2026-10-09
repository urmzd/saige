package research

import "github.com/urmzd/saige/tools/internal/safepath"

// resolveWithinRoot confines a requested path to root. See safepath.Resolve.
func resolveWithinRoot(root, requested string, requireExist bool) (string, error) {
	return safepath.Resolve(root, requested, requireExist)
}
