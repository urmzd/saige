package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// Vault swaps sensitive values for placeholders and back. One vault serves
// one session, so a placeholder means the same value for the whole
// conversation and the model can refer to it across turns.
type Vault interface {
	// Tokenize replaces every detected value in text with its placeholder.
	Tokenize(ctx context.Context, text string) (string, error)
	// Restore replaces every placeholder this vault issued with its value.
	// Placeholders it did not issue are left as they are.
	Restore(text string) string
	// Snapshot serializes the mapping so a resumed run restores the same
	// placeholders. The snapshot holds the original values: store it with
	// the same protection as the data itself.
	Snapshot() ([]byte, error)
}

// placeholderRE matches a placeholder: <<LABEL_N>>.
var placeholderRE = regexp.MustCompile(`<<[A-Z0-9_]+_[0-9]+>>`)

// maxPlaceholderLen bounds how much text a streaming restorer holds back
// while it waits to see whether a "<<" starts a placeholder.
const maxPlaceholderLen = 64

// MemoryVault is an in-process Vault. It is safe for concurrent use.
type MemoryVault struct {
	detector Detector

	mu       sync.Mutex
	byValue  map[string]string // label + "\x00" + value -> placeholder
	byToken  map[string]string // placeholder -> value
	labels   map[string]string // placeholder -> label
	counters map[string]int    // label -> last number issued
	// sensitive makes Provider refuse opaque media and audio output by
	// default. It is configuration, not data, so it is not snapshotted.
	sensitive bool
}

var (
	_ Vault       = (*MemoryVault)(nil)
	_ Sensitivity = (*MemoryVault)(nil)
)

// NewVault returns an empty vault that finds values with d. A nil d uses
// DefaultDetector.
func NewVault(d Detector) *MemoryVault {
	if d == nil {
		d = DefaultDetector()
	}
	return &MemoryVault{
		detector: d,
		byValue:  map[string]string{},
		byToken:  map[string]string{},
		labels:   map[string]string{},
		counters: map[string]int{},
	}
}

// LoadVault rebuilds a vault from a Snapshot.
func LoadVault(d Detector, snapshot []byte) (*MemoryVault, error) {
	v := NewVault(d)
	var s vaultSnapshot
	if err := json.Unmarshal(snapshot, &s); err != nil {
		return nil, fmt.Errorf("privacy: decode vault snapshot: %w", err)
	}
	if s.Version != snapshotVersion {
		return nil, fmt.Errorf("privacy: unsupported vault snapshot version %d", s.Version)
	}
	for _, e := range s.Entries {
		if !placeholderRE.MatchString(e.Token) || placeholderRE.FindString(e.Token) != e.Token || !validLabel(e.Label) {
			return nil, fmt.Errorf("privacy: invalid vault entry %q", e.Token)
		}
		v.byValue[e.Label+"\x00"+e.Value] = e.Token
		v.byToken[e.Token] = e.Value
		v.labels[e.Token] = e.Label
	}
	for label, n := range s.Counters {
		v.counters[label] = n
	}
	return v, nil
}

// Tokenize implements Vault.
func (v *MemoryVault) Tokenize(ctx context.Context, text string) (string, error) {
	if text == "" {
		return text, nil
	}
	spans, err := detectChecked(ctx, v.detector, text)
	if err != nil {
		return "", err
	}
	if len(spans) == 0 {
		return text, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return rewrite(text, spans, func(s Span) string {
		return v.placeholderLocked(s.Label, text[s.Start:s.End])
	}), nil
}

func (v *MemoryVault) placeholderLocked(label, value string) string {
	key := label + "\x00" + value
	if tok, ok := v.byValue[key]; ok {
		return tok
	}
	v.counters[label]++
	tok := "<<" + label + "_" + strconv.Itoa(v.counters[label]) + ">>"
	v.byValue[key] = tok
	v.byToken[tok] = value
	v.labels[tok] = label
	return tok
}

// Restore implements Vault.
func (v *MemoryVault) Restore(text string) string {
	return v.RestoreEscaped(text, nil)
}

// RestoreEscaped restores placeholders, passing each value through escape
// first. Use it to restore inside an encoded fragment, such as streamed JSON
// argument text, where a raw value could break the encoding. A nil escape
// inserts values as they are.
func (v *MemoryVault) RestoreEscaped(text string, escape func(string) string) string {
	if len(text) < 5 {
		return text
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.byToken) == 0 {
		return text
	}
	return placeholderRE.ReplaceAllStringFunc(text, func(tok string) string {
		value, ok := v.byToken[tok]
		if !ok {
			return tok
		}
		if escape != nil {
			return escape(value)
		}
		return value
	})
}

// SetSensitive marks the vault as guarding data that must not leave as
// media: Provider then refuses opaque media unless its MediaPolicy says
// otherwise, and refuses audio output unless AllowAudioOut is set.
func (v *MemoryVault) SetSensitive(on bool) {
	v.mu.Lock()
	v.sensitive = on
	v.mu.Unlock()
}

// Sensitive implements Sensitivity.
func (v *MemoryVault) Sensitive() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sensitive
}

// Len returns how many distinct values the vault holds.
func (v *MemoryVault) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.byToken)
}

const snapshotVersion = 1

type vaultSnapshot struct {
	Version  int            `json:"v"`
	Entries  []vaultEntry   `json:"entries"`
	Counters map[string]int `json:"counters"`
}

type vaultEntry struct {
	Token string `json:"token"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// Snapshot implements Vault. Entries are sorted, so equal vaults produce
// equal snapshots.
func (v *MemoryVault) Snapshot() ([]byte, error) {
	v.mu.Lock()
	s := vaultSnapshot{Version: snapshotVersion, Entries: make([]vaultEntry, 0, len(v.byToken)), Counters: make(map[string]int, len(v.counters))}
	for tok, value := range v.byToken {
		s.Entries = append(s.Entries, vaultEntry{Token: tok, Label: v.labels[tok], Value: value})
	}
	for label, n := range v.counters {
		s.Counters[label] = n
	}
	v.mu.Unlock()
	sort.Slice(s.Entries, func(i, j int) bool { return s.Entries[i].Token < s.Entries[j].Token })
	return json.Marshal(s)
}
