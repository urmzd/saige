package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/urmzd/saige/agent/types"
)

// Merge layers overlays onto base, lowest first, and validates the result
// as a complete catalog. Neither input is modified.
//
// Model rows, templates and baselines merge field by field: the overlay row
// with the same key is an RFC 7396 merge patch on the base row, so a present
// key replaces, null deletes, an absent key keeps the base value, and arrays
// replace whole. add_capabilities and remove_capabilities accumulate across
// layers instead. "$replace": true replaces the whole row and "$delete": true
// removes it.
//
// Presets merge entry by entry: an overlay preset replaces the base preset
// with the same name whole, and null deletes it. To change one field, write
// a new preset that extends the base one.
//
// The revision becomes the layers' revisions joined with "+", the topmost
// default_preset wins, and a layer with inherit_default false drops every
// layer below it.
func Merge(base *Catalog, overlays ...*Catalog) (*Catalog, error) {
	out, err := merge(base, overlays...)
	if err != nil {
		return nil, err
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// merge layers without the final validation.
func merge(base *Catalog, overlays ...*Catalog) (*Catalog, error) {
	if base == nil {
		base = &Catalog{Version: SchemaVersion}
	}
	out := base.clone()
	out.InheritDefault = nil
	out.deletedPresets = nil
	for i, o := range overlays {
		if o == nil {
			continue
		}
		if o.Version != out.Version && out.Version != 0 {
			return nil, &ValidationError{Source: fmt.Sprintf("layer %d", i+1), Issues: []Issue{{Path: "version", Code: CodeVersion,
				Message: fmt.Sprintf("version %d does not match the base version %d", o.Version, out.Version), Severity: SeverityError}}}
		}
		if o.InheritDefault != nil && !*o.InheritDefault {
			out = &Catalog{Version: o.Version}
		}
		if err := out.apply(o); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// apply merges one overlay layer into c.
func (c *Catalog) apply(o *Catalog) error {
	if c.Version == 0 {
		c.Version = o.Version
	}
	if o.Schema != "" {
		c.Schema = o.Schema
	}
	switch {
	case o.Revision == "":
	case c.Revision == "":
		c.Revision = o.Revision
	default:
		c.Revision += "+" + o.Revision
	}
	if o.DefaultPreset != "" {
		c.DefaultPreset = o.DefaultPreset
	}
	if o.Dials != nil {
		var base types.Dials
		if c.Dials != nil {
			base = *c.Dials
		}
		d := base.Merge(*o.Dials)
		c.Dials = &d
	}
	var err error
	if c.Templates, err = patchMap(c.Templates, o.Templates); err != nil {
		return err
	}
	if c.Baselines, err = patchMap(c.Baselines, o.Baselines); err != nil {
		return err
	}
	for _, m := range o.Models {
		k := rowKey(m.Provider, m.Prefix)
		i := slices.IndexFunc(c.Models, func(b ModelSpec) bool { return rowKey(b.Provider, b.Prefix) == k })
		switch {
		case m.Delete:
			if i >= 0 {
				c.Models = slices.Delete(c.Models, i, i+1)
			}
		case i < 0 || m.Replace:
			row := m.clone()
			row.Replace, row.raw = false, nil
			if i < 0 {
				c.Models = append(c.Models, row)
			} else {
				c.Models[i] = row
			}
		default:
			row, err := patchSpec(c.Models[i], m)
			if err != nil {
				return fmt.Errorf("models %s: %w", k, err)
			}
			c.Models[i] = row
		}
	}
	sortModels(c.Models)
	if len(o.Presets) > 0 && c.Presets == nil {
		c.Presets = map[string]PresetSpec{}
	}
	for name, p := range o.Presets {
		c.Presets[name] = p.clone()
	}
	for _, name := range o.deletedPresets {
		delete(c.Presets, name)
	}
	return nil
}

func sortModels(m []ModelSpec) {
	sort.SliceStable(m, func(i, j int) bool { return rowKey(m[i].Provider, m[i].Prefix) < rowKey(m[j].Provider, m[j].Prefix) })
}

// patchMap merges overlay specs into base by name. A null value in the
// overlay file deletes the name.
func patchMap(base, over map[string]ModelSpec) (map[string]ModelSpec, error) {
	if len(over) == 0 {
		return base, nil
	}
	if base == nil {
		base = map[string]ModelSpec{}
	}
	for name, o := range over {
		if string(bytes.TrimSpace(o.raw)) == jsonNull {
			delete(base, name)
			continue
		}
		b, ok := base[name]
		if !ok {
			base[name] = o.clone()
			continue
		}
		merged, err := patchSpec(b, o)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		base[name] = merged
	}
	return base, nil
}

// patchSpec applies overlay o to base b as a JSON merge patch.
func patchSpec(b, o ModelSpec) (ModelSpec, error) {
	baseRaw, err := json.Marshal(b)
	if err != nil {
		return ModelSpec{}, err
	}
	overRaw := o.raw
	if len(overRaw) == 0 {
		if overRaw, err = json.Marshal(o); err != nil {
			return ModelSpec{}, err
		}
	}
	// Decode numbers as json.Number so a value round-trips digit for digit:
	// a float64 would round integers above 2^53 and change decimals.
	bm, err := decodeObject(baseRaw)
	if err != nil {
		return ModelSpec{}, err
	}
	om, err := decodeObject(overRaw)
	if err != nil {
		return ModelSpec{}, err
	}
	delete(om, "$replace")
	delete(om, "$delete")
	add, _ := om["add_capabilities"].([]any)
	remove, _ := om["remove_capabilities"].([]any)
	delete(om, "add_capabilities")
	delete(om, "remove_capabilities")
	merged := mergePatch(bm, om).(map[string]any)
	// Capability edits accumulate: the overlay's additions and removals are
	// applied on top of the base row's.
	merged["add_capabilities"] = editList(merged["add_capabilities"], add, remove)
	merged["remove_capabilities"] = editList(merged["remove_capabilities"], remove, add)
	data, err := json.Marshal(merged)
	if err != nil {
		return ModelSpec{}, err
	}
	var out ModelSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return ModelSpec{}, err
	}
	return out, nil
}

// decodeObject decodes a JSON object with numbers kept as json.Number.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// editList returns list plus add minus remove, without duplicates.
func editList(list any, add, remove []any) any {
	cur, _ := list.([]any)
	var out []any
	has := func(xs []any, v any) bool { return slices.Contains(xs, v) }
	for _, v := range append(append([]any(nil), cur...), add...) {
		if !has(out, v) && !has(remove, v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergePatch implements RFC 7396.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}

// clone deep-copies a catalog through its canonical JSON.
func (c *Catalog) clone() *Catalog {
	data, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("catalog: clone: %v", err))
	}
	out := &Catalog{}
	if err := json.Unmarshal(data, out); err != nil {
		panic(fmt.Sprintf("catalog: clone: %v", err))
	}
	out.deletedPresets = append([]string(nil), c.deletedPresets...)
	for _, name := range out.deletedPresets {
		delete(out.Presets, name)
	}
	// The JSON round trip drops each row's raw form, which is what tells a
	// merge an explicit null from an absent key, and turns a template or
	// baseline deleted with null into an empty one. Carry the raw forms
	// over. MarshalJSON writes rows sorted, so match them in that order.
	sorted := append([]ModelSpec(nil), c.Models...)
	sortModels(sorted)
	for i := range out.Models {
		if i < len(sorted) {
			out.Models[i].raw = cloneRaw(sorted[i].raw)
		}
	}
	copyRaw(out.Templates, c.Templates)
	copyRaw(out.Baselines, c.Baselines)
	return out
}

func cloneRaw(r json.RawMessage) json.RawMessage {
	if r == nil {
		return nil
	}
	return append(json.RawMessage(nil), r...)
}

// copyRaw copies each spec's raw form from src onto the same name in dst.
func copyRaw(dst, src map[string]ModelSpec) {
	for name, m := range src {
		if d, ok := dst[name]; ok {
			d.raw = cloneRaw(m.raw)
			dst[name] = d
		}
	}
}

func (s ModelSpec) clone() ModelSpec {
	data, _ := json.Marshal(s)
	var out ModelSpec
	_ = json.Unmarshal(data, &out)
	out.raw = cloneRaw(s.raw)
	return out
}

func (p PresetSpec) clone() PresetSpec {
	data, _ := json.Marshal(p)
	var out PresetSpec
	_ = json.Unmarshal(data, &out)
	return out
}

// MarshalJSON writes the catalog canonically: rows sorted by provider and
// prefix, map keys sorted, and presets deleted by this layer written as null.
func (c *Catalog) MarshalJSON() ([]byte, error) {
	type plain Catalog
	p := plain(*c)
	p.Models = append([]ModelSpec(nil), c.Models...)
	sortModels(p.Models)
	if len(c.deletedPresets) == 0 {
		return marshalNoEscape(p)
	}
	presets := map[string]*PresetSpec{}
	for name, ps := range c.Presets {
		presets[name] = &ps
	}
	for _, name := range c.deletedPresets {
		presets[name] = nil
	}
	type withNulls struct {
		plain
		Presets map[string]*PresetSpec `json:"presets,omitempty"`
	}
	return marshalNoEscape(withNulls{plain: p, Presets: presets})
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Canonical returns the catalog as indented canonical JSON, the form
// `saige catalog export` writes and the golden tests compare.
func (c *Catalog) Canonical() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Clone returns a deep copy of the catalog.
func (c *Catalog) Clone() *Catalog { return c.clone() }
