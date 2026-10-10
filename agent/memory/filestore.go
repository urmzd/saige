package memory

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoriesRoot is the virtual directory the file commands operate under.
const MemoriesRoot = "/memories"

// MaxFileBytes bounds one memory file.
const MaxFileBytes = 256 << 10

// metaPrefix and metaSuffix delimit the header line Remember writes. The
// header is JSON encoded with HTML escaping, so it cannot contain the suffix.
const (
	metaPrefix = "<!-- saige-memory "
	metaSuffix = " -->"
)

// FileStore keeps memories as files, one directory per scope:
// <root>/<tenant>/<subject>/<namespace>/. Within a scope directory the files
// are what the model sees under /memories. Remember writes a markdown file
// with a one-line metadata header; files written through the commands are
// recalled too, as semantic memories.
//
// Every access goes through os.Root, so a path or link cannot leave its
// scope directory, and every write goes to a temporary file that is renamed
// into place. Commands that read and rewrite a file are serialized within
// the process; separate processes sharing a root should not edit the same
// file at once.
type FileStore struct {
	root string
	now  func() time.Time
	mu   sync.Mutex
}

var (
	_ Store     = (*FileStore)(nil)
	_ Commander = (*FileStore)(nil)
)

// NewFileStore opens or creates a store at root.
func NewFileStore(root string) (*FileStore, error) {
	if root == "" {
		return nil, fmt.Errorf("memory: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &FileStore{root: abs, now: time.Now}, nil
}

// escapeSegment maps any string to one safe path segment: letters, digits,
// "-" and "_" are kept, every other byte is written as %XX, and the empty
// string is "~". The mapping is reversible, so distinct scopes never share
// a directory.
func escapeSegment(s string) string {
	if s == "" {
		return "~"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func unescapeSegment(s string) (string, bool) {
	if s == "~" {
		return "", true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		v, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil {
			return "", false
		}
		b.WriteByte(v[0])
		i += 2
	}
	return b.String(), true
}

func (f *FileStore) subjectDir(s Scope) string {
	return filepath.Join(f.root, escapeSegment(s.Tenant), escapeSegment(s.Subject))
}

func (f *FileStore) scopeDir(s Scope) string {
	return filepath.Join(f.subjectDir(s), escapeSegment(s.Namespace))
}

// openScope opens the scope directory, creating it when create is set. A
// missing directory without create returns ErrNotFound.
func (f *FileStore) openScope(s Scope, create bool) (*os.Root, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	dir := f.scopeDir(s)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return r, err
}

// visibleScopes lists s and every stored sub-namespace of it.
func (f *FileStore) visibleScopes(s Scope) ([]Scope, error) {
	entries, err := os.ReadDir(f.subjectDir(s))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Scope
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ns, ok := unescapeSegment(e.Name())
		if !ok {
			continue
		}
		o := Scope{Tenant: s.Tenant, Subject: s.Subject, Namespace: ns}
		if s.contains(o) {
			out = append(out, o)
		}
	}
	return out, nil
}

type fileMeta struct {
	ID             string     `json:"id,omitempty"`
	Kind           Kind       `json:"kind,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
	Source         Provenance `json:"source,omitempty"`
	Created        time.Time  `json:"created,omitempty"`
	Expires        time.Time  `json:"expires,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

func encodeRecord(r Record) ([]byte, error) {
	meta, err := json.Marshal(fileMeta{ID: r.ID, Kind: r.Kind, Tags: r.Tags, Source: r.Source, Created: r.CreatedAt, Expires: r.ExpiresAt, IdempotencyKey: r.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	return []byte(metaPrefix + string(meta) + metaSuffix + "\n" + r.Content), nil
}

// decodeRecord parses a file. A file without a header is a semantic memory
// whose content is the whole file.
func decodeRecord(id string, data []byte, mod time.Time) Record {
	r := Record{ID: id, Kind: KindSemantic, Content: string(data), CreatedAt: mod.UTC()}
	first, rest, _ := strings.Cut(string(data), "\n")
	if !strings.HasPrefix(first, metaPrefix) || !strings.HasSuffix(first, metaSuffix) {
		return r
	}
	var m fileMeta
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(first, metaPrefix), metaSuffix)), &m); err != nil {
		return r
	}
	r.Content = rest
	if m.Kind.Valid() {
		r.Kind = m.Kind
	}
	r.Tags, r.Source, r.ExpiresAt, r.IdempotencyKey = m.Tags, m.Source, m.Expires, m.IdempotencyKey
	if !m.Created.IsZero() {
		r.CreatedAt = m.Created
	}
	return r
}

// writeFile writes data to name inside root through a temporary file.
func writeFile(root *os.Root, name string, data []byte) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(name), ".tmp-"+hex.EncodeToString(suffix[:]))
	fh, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := fh.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// Remember implements Store. The record is written to <id>.md.
func (f *FileStore) Remember(ctx context.Context, r Record) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.Scope.ReadOnly {
		return "", ErrReadOnly
	}
	if r.Kind == "" {
		r.Kind = KindSemantic
	}
	if r.ID == "" {
		r.ID = recordID(r) + ".md"
	}
	if _, err := cleanRel(r.ID); err != nil || r.ID == "." {
		return "", fmt.Errorf("memory: invalid id %q", r.ID)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = f.now().UTC()
	}
	data, err := encodeRecord(r)
	if err != nil {
		return "", err
	}
	if len(data) > MaxFileBytes {
		return "", fmt.Errorf("memory: record larger than %d bytes", MaxFileBytes)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	root, err := f.openScope(r.Scope, true)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if r.IdempotencyKey != "" {
		if _, err := root.Stat(r.ID); err == nil {
			return r.ID, nil
		}
	}
	if err := writeFile(root, r.ID, data); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Recall implements Store.
func (f *FileStore) Recall(ctx context.Context, s Scope, query string, budget int) ([]Record, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	scopes, err := f.visibleScopes(s)
	if err != nil {
		return nil, err
	}
	var all []Record
	for _, sc := range scopes {
		recs, err := f.readScope(ctx, sc)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
	}
	return rankRecords(all, query, budget, f.now()), nil
}

func (f *FileStore) readScope(ctx context.Context, s Scope) ([]Record, error) {
	root, err := f.openScope(s, false)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var out []Record
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := root.ReadFile(p)
		if err != nil {
			return err
		}
		r := decodeRecord(p, data, info.ModTime())
		r.Scope = s
		out = append(out, r)
		return nil
	})
	return out, err
}

// Forget implements Store. id is the record's path relative to /memories.
func (f *FileStore) Forget(ctx context.Context, s Scope, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ReadOnly {
		return ErrReadOnly
	}
	rel, err := cleanRel(id)
	if err != nil || rel == "." {
		return fmt.Errorf("memory: invalid id %q", id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := s.Validate(); err != nil {
		return err
	}
	// Recall from s also returns records from its sub-namespaces, so Forget
	// looks there too: s itself first, then each sub-namespace in order.
	scopes, err := f.visibleScopes(s)
	if err != nil {
		return err
	}
	sort.SliceStable(scopes, func(i, j int) bool { return scopes[i].Namespace == s.Namespace && scopes[j].Namespace != s.Namespace })
	for _, vs := range scopes {
		removed, err := f.removeIn(vs, rel)
		if err != nil {
			return err
		}
		if removed {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// removeIn removes rel from the directory of s and reports whether it was
// there.
func (f *FileStore) removeIn(s Scope, rel string) (bool, error) {
	root, err := f.openScope(s, false)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// cleanRel validates a path relative to /memories.
func cleanRel(p string) (string, error) {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("memory: invalid path %q", p)
	}
	clean := path.Clean(p)
	for _, part := range strings.Split(clean, "/") {
		if part == ".." || strings.HasPrefix(part, ".tmp-") {
			return "", fmt.Errorf("memory: invalid path %q", p)
		}
	}
	return clean, nil
}

// memPath maps a /memories path to a path relative to the scope directory.
func memPath(p string) (string, error) {
	if p == MemoriesRoot || p == MemoriesRoot+"/" {
		return ".", nil
	}
	rest, ok := strings.CutPrefix(p, MemoriesRoot+"/")
	if !ok {
		return "", fmt.Errorf("memory: path must be under %s: %q", MemoriesRoot, p)
	}
	return cleanRel(rest)
}

func displayPath(rel string) string {
	if rel == "." {
		return MemoriesRoot
	}
	return MemoriesRoot + "/" + rel
}

// Memory command names, the values of Command.Command.
const (
	CommandView       = "view"
	CommandCreate     = "create"
	CommandStrReplace = "str_replace"
	CommandInsert     = "insert"
	CommandDelete     = "delete"
	CommandRename     = "rename"
)

// Command is one file command under /memories. The fields used depend on
// Command: view (Path, ViewRange), create (Path, FileText), str_replace
// (Path, OldStr, NewStr), insert (Path, InsertLine, InsertText), delete
// (Path), and rename (OldPath, NewPath).
type Command struct {
	Command    string `json:"command"`
	Path       string `json:"path,omitempty"`
	ViewRange  []int  `json:"view_range,omitempty"`
	FileText   string `json:"file_text,omitempty"`
	OldStr     string `json:"old_str,omitempty"`
	NewStr     string `json:"new_str,omitempty"`
	InsertLine int    `json:"insert_line,omitempty"`
	InsertText string `json:"insert_text,omitempty"`
	OldPath    string `json:"old_path,omitempty"`
	NewPath    string `json:"new_path,omitempty"`
}

// Writes reports whether the command changes files.
func (c Command) Writes() bool { return c.Command != CommandView }

// Commander runs file commands in a scope.
type Commander interface {
	Run(ctx context.Context, s Scope, c Command) (string, error)
}

// Run implements Commander.
//
//nolint:gocyclo // one case per memory command
func (f *FileStore) Run(ctx context.Context, s Scope, c Command) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.Validate(); err != nil {
		return "", err
	}
	switch c.Command {
	case CommandView, CommandCreate, CommandStrReplace, CommandInsert, CommandDelete, CommandRename:
	default:
		return "", fmt.Errorf("memory: unknown command %q", c.Command)
	}
	if c.Writes() && s.ReadOnly {
		return "", ErrReadOnly
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	root, err := f.openScope(s, c.Writes())
	if errors.Is(err, ErrNotFound) && c.Command == CommandView && (c.Path == MemoriesRoot || c.Path == MemoriesRoot+"/") {
		return "Directory " + MemoriesRoot + " is empty.", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	switch c.Command {
	case CommandView:
		return viewPath(root, c)
	case CommandCreate:
		rel, err := memPath(c.Path)
		if err != nil {
			return "", err
		}
		if rel == "." {
			return "", fmt.Errorf("memory: cannot create %s", MemoriesRoot)
		}
		if len(c.FileText) > MaxFileBytes {
			return "", fmt.Errorf("memory: file larger than %d bytes", MaxFileBytes)
		}
		if err := writeFile(root, rel, []byte(c.FileText)); err != nil {
			return "", err
		}
		return "File created successfully at: " + displayPath(rel), nil
	case CommandStrReplace:
		rel, data, err := readTarget(root, c.Path)
		if err != nil {
			return "", err
		}
		if c.OldStr == "" {
			return "", fmt.Errorf("memory: old_str is required")
		}
		switch n := strings.Count(string(data), c.OldStr); n {
		case 0:
			return "", fmt.Errorf("memory: old_str not found in %s", displayPath(rel))
		case 1:
		default:
			return "", fmt.Errorf("memory: old_str occurs %d times in %s; it must be unique", n, displayPath(rel))
		}
		out := strings.Replace(string(data), c.OldStr, c.NewStr, 1)
		if len(out) > MaxFileBytes {
			return "", fmt.Errorf("memory: file larger than %d bytes", MaxFileBytes)
		}
		if err := writeFile(root, rel, []byte(out)); err != nil {
			return "", err
		}
		return "The memory file has been edited: " + displayPath(rel), nil
	case CommandInsert:
		rel, data, err := readTarget(root, c.Path)
		if err != nil {
			return "", err
		}
		lines := strings.SplitAfter(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if c.InsertLine < 0 || c.InsertLine > len(lines) {
			return "", fmt.Errorf("memory: insert_line %d out of range [0, %d]", c.InsertLine, len(lines))
		}
		text := c.InsertText
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if c.InsertLine > 0 && !strings.HasSuffix(lines[c.InsertLine-1], "\n") {
			lines[c.InsertLine-1] += "\n"
		}
		out := strings.Join(lines[:c.InsertLine], "") + text + strings.Join(lines[c.InsertLine:], "")
		if len(out) > MaxFileBytes {
			return "", fmt.Errorf("memory: file larger than %d bytes", MaxFileBytes)
		}
		if err := writeFile(root, rel, []byte(out)); err != nil {
			return "", err
		}
		return "Text inserted at line " + fmt.Sprint(c.InsertLine) + " in " + displayPath(rel), nil
	case CommandDelete:
		rel, err := memPath(c.Path)
		if err != nil {
			return "", err
		}
		if rel == "." {
			return "", fmt.Errorf("memory: cannot delete %s", MemoriesRoot)
		}
		if _, err := root.Lstat(rel); err != nil {
			return "", fmt.Errorf("%w: %s", ErrNotFound, displayPath(rel))
		}
		if err := root.RemoveAll(rel); err != nil {
			return "", err
		}
		return "Deleted: " + displayPath(rel), nil
	default: // rename
		from, err := memPath(c.OldPath)
		if err != nil {
			return "", err
		}
		to, err := memPath(c.NewPath)
		if err != nil {
			return "", err
		}
		if from == "." || to == "." {
			return "", fmt.Errorf("memory: cannot rename %s", MemoriesRoot)
		}
		if _, err := root.Lstat(from); err != nil {
			return "", fmt.Errorf("%w: %s", ErrNotFound, displayPath(from))
		}
		if _, err := root.Lstat(to); err == nil {
			return "", fmt.Errorf("memory: %s already exists", displayPath(to))
		}
		if dir := path.Dir(to); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
		}
		if err := root.Rename(from, to); err != nil {
			return "", err
		}
		return "Renamed " + displayPath(from) + " to " + displayPath(to), nil
	}
}

func readTarget(root *os.Root, p string) (string, []byte, error) {
	rel, err := memPath(p)
	if err != nil {
		return "", nil, err
	}
	data, err := root.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("%w: %s", ErrNotFound, displayPath(rel))
	}
	return rel, data, err
}

func viewPath(root *os.Root, c Command) (string, error) {
	rel, err := memPath(c.Path)
	if err != nil {
		return "", err
	}
	info, err := root.Stat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, displayPath(rel))
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		var lines []string
		err := fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == rel || strings.HasPrefix(d.Name(), ".tmp-") {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			name := displayPath(p)
			if d.IsDir() {
				name += "/"
			}
			lines = append(lines, fmt.Sprintf("%d\t%s", fi.Size(), name))
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(lines)
		if len(lines) == 0 {
			return "Directory " + displayPath(rel) + " is empty.", nil
		}
		return "Contents of " + displayPath(rel) + ":\n" + strings.Join(lines, "\n"), nil
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return "", err
	}
	start, end := 1, -1
	if len(c.ViewRange) == 2 {
		start, end = c.ViewRange[0], c.ViewRange[1]
	} else if len(c.ViewRange) != 0 {
		return "", fmt.Errorf("memory: view_range must be [start, end]")
	}
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64<<10), MaxFileBytes+1)
	n := 0
	for sc.Scan() {
		n++
		if n < start || (end >= 0 && n > end) {
			continue
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
