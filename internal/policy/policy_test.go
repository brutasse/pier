package policy

import (
	"testing"
)

var testRules = []Rule{
	{
		Name:   "publish-from-tags",
		Action: []string{"upload"},
		When:   `claims.repository == "acme/widget" && claims.sub.startsWith("repo:acme/widget:ref:refs/tags/")`,
	},
	{
		Name:   "ci-download",
		Action: []string{"download"},
		When:   `claims.repository in ["acme/widget", "acme/other"]`,
	},
	{
		Name:   "ops-download",
		Action: []string{"download", "delete"},
		When:   `claims.sub == "ops@acme.test"`,
	},
}

func act(repository, sub string) map[string]any {
	claims := map[string]any{}
	if repository != "" {
		claims["repository"] = repository
	}
	if sub != "" {
		claims["sub"] = sub
	}
	return map[string]any{
		"claims":   claims,
		"group":    "com.acme",
		"artifact": "widget",
		"version":  "1.0.0",
		"snapshot": false,
		"path":     "com/acme/widget/1.0.0/widget-1.0.0.jar",
	}
}

func TestEval(t *testing.T) {
	p, err := New(testRules, true)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		action   string
		activ    map[string]any
		wantRule string
	}{
		{"publish from tag", "upload", act("acme/widget", "repo:acme/widget:ref:refs/tags/v1.0.0"), "publish-from-tags"},
		{"publish from branch denied", "upload", act("acme/widget", "repo:acme/widget:ref:refs/heads/main"), ""},
		{"publish from other repo denied", "upload", act("other/thing", "repo:other/thing:ref:refs/tags/v1.0.0"), ""},
		{"ci download", "download", act("acme/widget", "repo:acme/widget:ref:refs/heads/main"), "ci-download"},
		{"ci download other repo", "download", act("acme/other", "repo:acme/other:ref:refs/heads/main"), "ci-download"},
		{"ci download unknown repo denied", "download", act("acme/unknown", "repo:acme/unknown:ref:refs/heads/main"), ""},
		{"ops download", "download", act("", "ops@acme.test"), "ops-download"},
		{"ops delete", "delete", act("", "ops@acme.test"), "ops-download"},
		{"unknown user denied", "download", act("", "nobody@acme.test"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.activ["action"] = c.action
			got, err := p.Eval(c.action, c.activ)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.wantRule {
				t.Fatalf("rule = %q, want %q", got, c.wantRule)
			}
		})
	}
}

// A rule that references a claim the token does not carry must count as a
// non-match, not as an error.
func TestEvalMissingClaimSkipsRule(t *testing.T) {
	p, err := New([]Rule{
		{Name: "human", When: `claims.email == "alice@example.io"`},
		{Name: "any", When: `claims.sub == "repo:acme/widget:ref:refs/heads/main"`},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := p.Eval("download", act("acme/widget", "repo:acme/widget:ref:refs/heads/main"))
	if got != "any" {
		t.Fatalf("rule = %q, want %q", got, "any")
	}
}

func TestEvalDefaultAllow(t *testing.T) {
	p, err := New(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Eval("download", act("", "x")); got == "" {
		t.Fatal("expected default-allow, got deny")
	}
}

func TestNewRejectsInvalid(t *testing.T) {
	cases := []struct {
		name  string
		rules []Rule
		deny  bool
		frag  string
	}{
		{"no name", []Rule{{When: `true`}}, true, "without a name"},
		{"duplicate name", []Rule{{Name: "a", When: "true"}, {Name: "a", When: "true"}}, true, "duplicate"},
		{"no expression", []Rule{{Name: "a"}}, true, "no expression"},
		{"unknown action", []Rule{{Name: "a", Action: []string{"nuke"}, When: "true"}}, true, "unknown action"},
		{"bad expression", []Rule{{Name: "a", When: "this is not cel"}}, true, "rule \"a\""},
		{"unknown variable", []Rule{{Name: "a", When: "undeclared_var == 1"}}, true, "rule \"a\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.rules, c.deny)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
