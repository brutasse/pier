// Package policy evaluates CEL authorization rules.
//
// Rules are evaluated in order; the first one whose expression is true
// allows the action, otherwise the request is denied. A rule that errors
// during evaluation (typically because the token lacks a claim the rule
// references) counts as a non-match.
package policy

import (
	"errors"
	"fmt"

	"cel.dev/cel-go/cel"
)

// Rule is one authorization rule.
type Rule struct {
	Name   string   `yaml:"name"`
	Action []string `yaml:"action"` // empty = all actions
	When   string   `yaml:"when"`
}

var knownActions = map[string]bool{"download": true, "upload": true, "delete": true}

type rule struct {
	name    string
	actions map[string]bool
	prog    cel.Program
}

// Policy is a compiled set of rules.
type Policy struct {
	defaultDeny bool
	rules       []rule
}

// New compiles the rules. It returns an error if any rule is invalid.
func New(rules []Rule, defaultDeny bool) (*Policy, error) {
	env, err := cel.NewEnv(
		cel.Variable("claims", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("action", cel.StringType),
		cel.Variable("group", cel.StringType),
		cel.Variable("artifact", cel.StringType),
		cel.Variable("version", cel.StringType),
		cel.Variable("snapshot", cel.BoolType),
		cel.Variable("path", cel.StringType),
	)
	if err != nil {
		return nil, err
	}
	p := &Policy{defaultDeny: defaultDeny}
	seen := make(map[string]bool, len(rules))
	for _, rc := range rules {
		if rc.Name == "" {
			return nil, errors.New("rule without a name")
		}
		if seen[rc.Name] {
			return nil, fmt.Errorf("duplicate rule name %q", rc.Name)
		}
		seen[rc.Name] = true
		if rc.When == "" {
			return nil, fmt.Errorf("rule %q has no expression", rc.Name)
		}
		actions := make(map[string]bool, len(rc.Action))
		for _, a := range rc.Action {
			if !knownActions[a] {
				return nil, fmt.Errorf("rule %q: unknown action %q", rc.Name, a)
			}
			actions[a] = true
		}
		ast, iss := env.Compile(rc.When)
		if iss != nil && iss.Err() != nil {
			return nil, fmt.Errorf("rule %q: %v", rc.Name, iss.Err())
		}
		prog, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", rc.Name, err)
		}
		p.rules = append(p.rules, rule{name: rc.Name, actions: actions, prog: prog})
	}
	return p, nil
}

// Eval returns the name of the first rule that allows the action, or ""
// when no rule matches and the policy denies by default.
func (p *Policy) Eval(action string, activation map[string]any) (string, error) {
	for _, r := range p.rules {
		if len(r.actions) > 0 && !r.actions[action] {
			continue
		}
		out, _, err := r.prog.Eval(activation)
		if err != nil {
			continue
		}
		if b, ok := out.Value().(bool); ok && b {
			return r.name, nil
		}
	}
	if p.defaultDeny {
		return "", nil
	}
	return "default-allow", nil
}
