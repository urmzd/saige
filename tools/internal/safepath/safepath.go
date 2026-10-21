// Package safepath confines tool paths to a workspace root.
//
// Open is the race-free form: it returns an os.Root, which checks every
// path inside the root at the moment of use. Resolve checks a path once and
// returns it, so a symlink swapped in between the check and the use can
// still lead out; it remains for callers that hand a path to another
// process, such as a subprocess's working directory.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Open opens root as an os.Root and returns it with requested as a path
// relative to it (see Rel). Every operation through the Root resolves its
// path inside root when the operation is made, following a symlink only
// while it stays inside root, so there is no window between a check and a
// use in which a swapped symlink could lead out. An absolute symlink is
// refused even when it points inside root. The caller closes the Root.
func Open(root, requested string) (*os.Root, string, error) {
	rel, err := Rel(root, requested)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("open root: %w", err)
	}
	return r, rel, nil
}

// Rel returns requested as a clean path relative to root ("." for root
// itself), refusing a path that escapes root lexically: "../" traversal or
// an absolute path outside root. It reads nothing from disk, so it says
// nothing about symlinks; use the result with an os.Root (see Open).
func Rel(root, requested string) (string, error) {
	if requested == "" {
		return "", fmt.Errorf("path is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	target := requested
	if !filepath.IsAbs(target) {
		target = filepath.Join(absRoot, target)
	}
	target = filepath.Clean(target)
	if !Within(absRoot, target) {
		return "", fmt.Errorf("path escapes root %q: %q", root, requested)
	}
	rel, err := filepath.Rel(absRoot, target)
	if err != nil {
		return "", fmt.Errorf("path escapes root %q: %q", root, requested)
	}
	return rel, nil
}

// Resolve confines a requested path to root. It rejects any path that
// escapes root via "../" traversal, an absolute path outside root, or a symlink
// whose real target lies outside root.
//
// The returned path is absolute and cleaned. requireExist controls whether the
// final path must exist for symlink resolution: when true, the whole path is
// resolved via filepath.EvalSymlinks; when false (e.g. a search root that may be
// created later) the longest existing ancestor is resolved instead so a
// not-yet-created leaf does not spuriously fail.
func Resolve(root, requested string, requireExist bool) (string, error) {
	if requested == "" {
		return "", fmt.Errorf("path is required")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	target := requested
	if !filepath.IsAbs(target) {
		target = filepath.Join(absRoot, target)
	}
	target = filepath.Clean(target)

	// Lexical containment check first (catches "../../etc/passwd" and absolute
	// paths outside root before any filesystem access). Compared without symlink
	// resolution so both sides are in the same (unresolved) namespace.
	if !Within(absRoot, target) {
		return "", fmt.Errorf("path escapes root %q: %q", root, requested)
	}

	// Symlink-aware check: resolve both the root and the target to their real
	// paths and re-verify containment, so a symlink inside root that points
	// outside root is rejected. Resolving root too keeps the comparison sound on
	// systems where the root itself sits under a symlink (e.g. /tmp -> /private/tmp).
	realRoot := absRoot
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		realRoot = r
	}
	real, err := evalSymlinksLenient(target, requireExist)
	if err != nil {
		return "", err
	}
	if !Within(realRoot, real) {
		return "", fmt.Errorf("symlink escapes root %q: %q", root, requested)
	}

	return target, nil
}

// Within reports whether target is root or a descendant of root.
func Within(root, target string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// evalSymlinksLenient resolves symlinks. If requireExist is false and the leaf
// does not exist, it resolves the longest existing ancestor and re-appends the
// remaining (non-existent) suffix, so paths to files that have not been created
// yet are still validated against symlink escapes in their parent chain.
func evalSymlinksLenient(target string, requireExist bool) (string, error) {
	real, err := filepath.EvalSymlinks(target)
	if err == nil {
		return real, nil
	}
	if requireExist {
		return "", fmt.Errorf("%w", err)
	}

	// Walk up to the longest existing ancestor, resolving it, then re-append the
	// missing suffix.
	dir := target
	var suffix []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the volume root with nothing resolvable.
			return target, nil
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent
		if realDir, derr := filepath.EvalSymlinks(dir); derr == nil {
			parts := append([]string{realDir}, suffix...)
			return filepath.Join(parts...), nil
		}
	}
}
