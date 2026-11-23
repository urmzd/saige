package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// readActionNames are the actions that only read and may name another
// issue, pull request or repository by "address".
var readActionNames = map[string]bool{
	"github_read_context":    true,
	"github_read_file":       true,
	"github_workflow_status": true,
	"github_fix_base":        true,
}

// oneKeyEach refuses a read action's call whose argument keys are
// ambiguous before anything is decoded or read. encoding/json matches a key
// to a field without regard to case and keeps the last of several that
// match, so {"address":"a","Address":"b"} would read b while a check of
// the first key read a. A key that two keys match, a key given twice, or a
// key the schema does not declare is refused. Arguments that are not one
// JSON object are left for the action's decoder to refuse. The bound
// target is checked first, as each action does, so a target the connector
// does not serve is refused as such.
func (c *Connector) oneKeyEach(action connector.Action) connector.Action {
	declared := declaredArguments(action.Schema)
	next := action.Invoke
	name := action.Name
	action.Invoke = func(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
		if target.Connector == Name && !isRepository(target) {
			if _, err := c.ref(target); err != nil {
				return connector.Result{}, err
			}
		}
		if err := checkArgumentKeys(name, raw, declared); err != nil {
			return connector.Result{}, err
		}
		return next(ctx, target, raw)
	}
	return action
}

// declaredArguments is the lower-cased property names a schema declares.
func declaredArguments(schema json.RawMessage) map[string]bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	out := make(map[string]bool, len(s.Properties))
	for name := range s.Properties {
		out[strings.ToLower(name)] = true
	}
	return out
}

func checkArgumentKeys(name string, raw json.RawMessage, declared map[string]bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil
		}
		key, _ := t.(string)
		lower := strings.ToLower(key)
		if !declared[lower] {
			return fmt.Errorf("github: %s takes no argument %q", name, key)
		}
		if seen[lower] {
			return fmt.Errorf("github: %s was given %q more than once", name, lower)
		}
		seen[lower] = true
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}
