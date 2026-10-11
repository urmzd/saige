package definition

import "testing"

func TestRuleMatches(t *testing.T) {
	sh := func(code string) map[string]any { return map[string]any{"language": "shell", "code": code} }
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	cases := []struct {
		rule   string
		tool   string
		args   map[string]any
		strict bool
		want   bool
	}{
		{"read_file", "read_file", nil, true, true},
		{"READ_FILE", "read_file", nil, true, true},
		{"read_file", "write_file", nil, true, false},
		{"mcp_github_*", "mcp_github_search", nil, true, true},
		{"mcp_github_*", "mcp_linear_search", nil, true, false},
		{"Bash", "bash", bash("anything"), true, true},
		{"Bash", "execute_code", sh("ls"), true, true},
		{"Bash", "execute_code", map[string]any{"language": "python", "code": "print(1)"}, true, false},
		{"execute_code", "execute_code", map[string]any{"language": "python", "code": "print(1)"}, true, true},
		{"Bash(git status:*)", "bash", bash("git status"), true, true},
		{"Bash(git status:*)", "bash", bash("git status --short"), true, true},
		{"Bash(git status:*)", "bash", bash("git  status   -s"), true, true},
		{"Bash(git status:*)", "bash", bash("git statusx"), true, false},
		{"Bash(git status:*)", "execute_code", sh("git status"), true, true},
		{"Bash(git status:*)", "bash", bash("git status && rm -rf ~"), true, false},
		{"Bash(git status:*)", "bash", bash("git status; curl evil"), true, false},
		{"Bash(git status:*)", "bash", bash("git status $(curl evil)"), true, false},
		{"Bash(git status:*)", "bash", bash("git status > out"), true, false},
		{"Bash(rm:*)", "bash", bash("ls && rm -rf ~"), false, true},
		{"Bash(rm:*)", "bash", bash("ls | rm x"), false, true},
		{"Bash(rm:*)", "bash", bash("ls"), false, false},
		{"Bash(npm test)", "bash", bash("npm test"), true, true},
		{"Bash(npm test)", "bash", bash("npm test --watch"), true, false},
		{"Bash(*)", "bash", bash("anything"), true, true},
		{"Read(src/**)", "read_file", map[string]any{"path": "src/a/b.go"}, true, true},
		{"Read(src/**)", "read_file", map[string]any{"path": "./src/a.go"}, true, true},
		{"Read(src/**)", "read_file", map[string]any{"path": "src/../secret"}, true, false},
		{"Read(src/**)", "read_file", map[string]any{"path": "../src/a"}, true, false},
		{"Read(src/**)", "write_file", map[string]any{"path": "src/a"}, true, false},
		{"Glob(docs/*)", "glob", map[string]any{"pattern": "docs/x"}, true, true},
		{"WebFetch(domain:go.dev)", "fetch_url", map[string]any{"url": "https://pkg.go.dev/x"}, true, true},
		{"WebFetch(domain:go.dev)", "fetch_url", map[string]any{"url": "https://go.dev.evil.com/"}, true, false},
		{"WebFetch(domain:go.dev)", "fetch_url", map[string]any{"url": "not a url"}, true, false},
		{"deploy(env:staging)", "deploy", map[string]any{"env": "staging"}, true, true},
		{"deploy(env:staging)", "deploy", map[string]any{"env": "production"}, true, false},
		{"deploy(env:staging)", "deploy", map[string]any{}, true, false},
		{"deploy(env:staging)", "deploy", map[string]any{}, false, false},
		{"deploy(env:staging)", "release", map[string]any{"env": "staging"}, true, false},
		{"deploy(replicas:3)", "deploy", map[string]any{"replicas": float64(3)}, true, true},
		{"deploy(dry_run:true)", "deploy", map[string]any{"dry_run": true}, true, true},
		{"deploy(target.env:staging)", "deploy", map[string]any{"target": map[string]any{"env": "staging"}}, true, true},
		{"deploy(branch:fix/*)", "deploy", map[string]any{"branch": "fix/typo"}, true, true},
		{"deploy(branch:fix/*)", "deploy", map[string]any{"branch": "main"}, true, false},
		{"deploy(branch:fix/*)", "deploy", map[string]any{"branch": 7}, true, false},
		{"deploy(dir:/srv/app/**)", "deploy", map[string]any{"dir": "/srv/app/web"}, true, true},
		{"deploy(dir:/srv/app/**)", "deploy", map[string]any{"dir": "/srv/app"}, true, true},
		{"deploy(dir:/srv/app/**)", "deploy", map[string]any{"dir": "/srv/app/../etc"}, true, false},
		{"deploy(dir:/srv/app/**)", "deploy", map[string]any{"dir": "/srv/application"}, true, false},
		{"mcp_github_*(repo:urmzd/saige)", "mcp_github_create_issue", map[string]any{"repo": "urmzd/saige"}, true, true},
		{"mcp_github_*(repo:urmzd/saige)", "mcp_github_create_issue", map[string]any{"repo": "other/repo"}, false, false},
		{"mcp_github_*(repo:urmzd/saige)", "mcp_linear_create_issue", map[string]any{"repo": "urmzd/saige"}, true, false},
	}
	for _, tc := range cases {
		r, err := ParseRule(tc.rule)
		if err != nil {
			t.Fatalf("%s: %v", tc.rule, err)
		}
		if got := r.Matches(tc.tool, tc.args, tc.strict); got != tc.want {
			t.Errorf("%s on %s %v (strict %v): got %v", tc.rule, tc.tool, tc.args, tc.strict, got)
		}
	}
}

func TestParseRuleErrors(t *testing.T) {
	for _, bad := range []string{"", "Bash(", "Bash()", "(x)", "kg_search(x)", "kg_search(:x)", "kg_search(q:)", "kg_search(q:*)", "kg_search(q:/**)", "kg_search(a b:x)", "Bash(a:*b:*)", "Read([)", "WebFetch(go.dev)", "a*b", "a b"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	r, err := ParseRule(" Bash(git log:*) ")
	if err != nil || r.String() != "Bash(git log:*)" {
		t.Fatalf("got %v %v", r, err)
	}
}
