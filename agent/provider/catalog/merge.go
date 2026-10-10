package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// Merge layers overlays onto base, lowest first, and validates the result
// as a complete catalog. Neither input is modified.
//
// Models, templates, endpoints and offerings merge field by field: the
// overlay value with the same key (an offering's key is its model and
// endpoint) is a merge patch on the base value, so a present key replaces,
// an absent key keeps the base value, and arrays replace whole. Null
// removes a whole model, template or endpoint; inside one, a null map
// entry (a parameter, a constraint, a modality, a tier) is kept, so it
// also removes what a template would supply. add_features and
// remove_features accumulate across layers. "$replace": true replaces the
// whole value and "$delete": true removes it; deleting a model removes its
// offerings.
//
// Presets merge entry by entry: an overlay preset replaces the base preset
// with the same name whole, and null deletes it. To change one field, write
// a new preset that extends the base one.
//
// The revision becomes the layers' revisions joined with "+", the topmost
// default_preset wins, and a layer with inherit_default false drops every
// layer below it. A version 1 layer is upgraded before it is merged.
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
	c.upgraded = c.upgraded || o.upgraded
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
	if c.ModelTemplates, err = patchMap(c.ModelTemplates, o.ModelTemplates); err != nil {
		return err
	}
	if c.OfferingTemplates, err = patchMap(c.OfferingTemplates, o.OfferingTemplates); err != nil {
		return err
	}
	if c.Endpoints, err = patchMap(c.Endpoints, o.Endpoints); err != nil {
		return err
	}
	for _, k := range sortedKeys(o.Models) {
		m := o.Models[k]
		b, ok := c.Models[k]
		switch {
		case m.Delete || isNullRaw(m.raw):
			delete(c.Models, k)
			c.Offerings = slices.DeleteFunc(c.Offerings, func(x OfferingSpec) bool { return x.Model == k })
			continue
		case !ok || m.Replace:
			m.Replace, m.raw = false, nil
		default:
			if m, err = patchValue(b, m); err != nil {
				return fmt.Errorf("models %s: %w", k, err)
			}
		}
		if c.Models == nil {
			c.Models = map[string]ModelSpec{}
		}
		c.Models[k] = m.cloneSpec()
	}
	for _, off := range o.Offerings {
		k := OfferingID(off.Model, off.Endpoint)
		i := slices.IndexFunc(c.Offerings, func(b OfferingSpec) bool { return OfferingID(b.Model, b.Endpoint) == k })
		switch {
		case off.Delete:
			if i >= 0 {
				c.Offerings = slices.Delete(c.Offerings, i, i+1)
			}
		case i < 0 || off.Replace:
			row := off.cloneSpec()
			row.Replace = false
			if i < 0 {
				c.Offerings = append(c.Offerings, row)
			} else {
				c.Offerings[i] = row
			}
		default:
			row, err := patchValue(c.Offerings[i], off)
			if err != nil {
				return fmt.Errorf("offerings %s: %w", k, err)
			}
			c.Offerings[i] = row
		}
	}
	sortOfferings(c.Offerings)
	if len(o.Presets) > 0 && c.Presets == nil {
		c.Presets = map[types.PresetName]PresetSpec{}
	}
	for name, p := range o.Presets {
		c.Presets[name] = p.clone()
	}
	for _, name := range o.deletedPresets {
		delete(c.Presets, name)
	}
	return nil
}

// patchable is a spec that keeps its raw form for merging.
type patchable[T any] interface {
	rawJSON() json.RawMessage
	withRaw(json.RawMessage) T
	isReplace() bool
}

func (m ModelSpec) rawJSON() json.RawMessage            { return m.raw }
func (m ModelSpec) withRaw(r json.RawMessage) ModelSpec { m.raw = r; return m }
func (m ModelSpec) isReplace() bool                     { return m.Replace }
func (m ModelSpec) cloneSpec() ModelSpec                { return cloneJSON(m).withRaw(cloneRaw(m.raw)) }
func (o OfferingSpec) rawJSON() json.RawMessage         { return o.raw }
func (o OfferingSpec) withRaw(r json.RawMessage) OfferingSpec {
	o.raw = r
	return o
}
func (o OfferingSpec) isReplace() bool          { return o.Replace }
func (o OfferingSpec) cloneSpec() OfferingSpec  { return cloneJSON(o).withRaw(cloneRaw(o.raw)) }
func (e EndpointSpec) rawJSON() json.RawMessage { return e.raw }
func (e EndpointSpec) isReplace() bool          { return false }
func (e EndpointSpec) withRaw(r json.RawMessage) EndpointSpec {
	e.raw = r
	return e
}

// cloneJSON deep-copies a value through its JSON form, keeping nulls in
// maps.
func cloneJSON[T any](v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("catalog: clone: %v", err))
	}
	var out T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		panic(fmt.Sprintf("catalog: clone: %v", err))
	}
	return out
}

// patchMap merges overlay values into base by name. A null value in the
// overlay file deletes the name.
func patchMap[T patchable[T]](base, over map[string]T) (map[string]T, error) {
	if len(over) == 0 {
		return base, nil
	}
	if base == nil {
		base = map[string]T{}
	}
	for _, name := range sortedKeys(over) {
		o := over[name]
		if isNullRaw(o.rawJSON()) {
			delete(base, name)
			continue
		}
		b, ok := base[name]
		if !ok || o.isReplace() {
			base[name] = cloneJSON(o).withRaw(cloneRaw(o.rawJSON()))
			continue
		}
		merged, err := patchValue(b, o)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		base[name] = merged
	}
	return base, nil
}

// patchValue applies overlay o to base b as a merge patch whose nulls are
// kept, so a null map entry still removes what a template supplies.
func patchValue[T patchable[T]](b, o T) (T, error) {
	var zero T
	baseRaw, err := json.Marshal(b)
	if err != nil {
		return zero, err
	}
	overRaw := o.rawJSON()
	if len(overRaw) == 0 {
		if overRaw, err = json.Marshal(o); err != nil {
			return zero, err
		}
	}
	// Decode numbers as json.Number so a value round-trips digit for digit:
	// a float64 would round integers above 2^53 and change decimals.
	bm, err := decodeObject(baseRaw)
	if err != nil {
		return zero, err
	}
	om, err := decodeObject(overRaw)
	if err != nil {
		return zero, err
	}
	delete(om, "$replace")
	delete(om, "$delete")
	add, _ := om["add_features"].([]any)
	remove, _ := om["remove_features"].([]any)
	delete(om, "add_features")
	delete(om, "remove_features")
	merged := mergeKeep(bm, om).(map[string]any)
	// Feature edits accumulate: the overlay's additions and removals are
	// applied on top of the base value's.
	if v := editList(merged["add_features"], add, remove); v != nil {
		merged["add_features"] = v
	} else {
		delete(merged, "add_features")
	}
	if v := editList(merged["remove_features"], remove, add); v != nil {
		merged["remove_features"] = v
	} else {
		delete(merged, "remove_features")
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return zero, err
	}
	var out T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return zero, err
	}
	return out.withRaw(data), nil
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
	for _, v := range append(append([]any(nil), cur...), add...) {
		if !slices.Contains(out, v) && !slices.Contains(remove, v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeKeep is RFC 7396 except that a null in the patch is kept in the
// result rather than deleting the key.
func mergeKeep(target, patch any) any {
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
			tm[k] = nil
			continue
		}
		tm[k] = mergeKeep(tm[k], v)
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		panic(fmt.Sprintf("catalog: clone: %v", err))
	}
	out.deletedPresets = slices.Clone(c.deletedPresets)
	out.upgraded = c.upgraded
	for _, name := range out.deletedPresets {
		delete(out.Presets, name)
	}
	// The JSON round trip drops each value's raw form, which is what tells
	// a merge a value deleted with null from an empty one. Carry the raw
	// forms over. MarshalJSON writes offerings sorted, so match them in
	// that order.
	sorted := slices.Clone(c.Offerings)
	sortOfferings(sorted)
	for i := range out.Offerings {
		if i < len(sorted) {
			out.Offerings[i].raw = cloneRaw(sorted[i].raw)
		}
	}
	copyRaw(out.ModelTemplates, c.ModelTemplates)
	copyRaw(out.Models, c.Models)
	copyRaw(out.Endpoints, c.Endpoints)
	copyRaw(out.OfferingTemplates, c.OfferingTemplates)
	return out
}

func cloneRaw(r json.RawMessage) json.RawMessage {
	if r == nil {
		return nil
	}
	return append(json.RawMessage(nil), r...)
}

// copyRaw copies each value's raw form from src onto the same name in dst.
func copyRaw[T patchable[T]](dst, src map[string]T) {
	for name, m := range src {
		if d, ok := dst[name]; ok {
			dst[name] = d.withRaw(cloneRaw(m.rawJSON()))
		}
	}
}

func (p PresetSpec) clone() PresetSpec {
	data, _ := json.Marshal(p)
	var out PresetSpec
	_ = json.Unmarshal(data, &out)
	return out
}

// MarshalJSON writes the catalog canonically: offerings sorted by model
// and endpoint, map keys sorted, and presets deleted by this layer written
// as null. A version 1 catalog upgraded in memory is written as version 2.
func (c *Catalog) MarshalJSON() ([]byte, error) {
	type plain Catalog
	p := plain(*c)
	p.Offerings = slices.Clone(c.Offerings)
	sortOfferings(p.Offerings)
	if len(c.deletedPresets) == 0 {
		return marshalNoEscape(p)
	}
	presets := map[types.PresetName]*PresetSpec{}
	for name, ps := range c.Presets {
		presets[name] = &ps
	}
	for _, name := range c.deletedPresets {
		presets[name] = nil
	}
	type withNulls struct {
		plain
		Presets map[types.PresetName]*PresetSpec `json:"presets,omitempty"`
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
