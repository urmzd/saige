package skills

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseSkillFile(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     Frontmatter
		wantBody string
		wantErr  bool
	}{
		{
			name:     "plain scalars",
			in:       "---\nname: demo\ndescription: Does a thing. Use when needed.\n---\n# Body\n",
			want:     Frontmatter{"name": "demo", "description": "Does a thing. Use when needed."},
			wantBody: "# Body\n",
		},
		{
			name: "quoted values and comments",
			in:   "---\n# a comment\nname: \"demo\"\ndescription: 'it''s fine' # trailing\nlicense: MIT # spdx\n---\n",
			want: Frontmatter{"name": "demo", "description": "it's fine", "license": "MIT"},
		},
		{
			name:     "nested metadata with flow value",
			in:       "---\nname: agent\nmetadata:\n  argument-hint: [task]\n  version: 0.1.0\n---\nbody",
			want:     Frontmatter{"name": "agent", "metadata": map[string]string{"argument-hint": "[task]", "version": "0.1.0"}},
			wantBody: "body",
		},
		{
			name: "flow and block lists",
			in:   "---\nallowed-tools: [read_file, \"web_search\"]\ntags:\n  - a\n  - 'b'\n---\n",
			want: Frontmatter{"allowed-tools": []string{"read_file", "web_search"}, "tags": []string{"a", "b"}},
		},
		{
			name: "folded and literal blocks",
			in:   "---\ndescription: >\n  first line\n  second line\n\n  new paragraph\nnotes: |-\n  keep\n    indent\n---\n",
			want: Frontmatter{"description": "first line second line\nnew paragraph\n", "notes": "keep\n  indent"},
		},
		{
			name: "plain scalar continued on indented lines",
			in:   "---\ndescription: a long\n  description here\n---\n",
			want: Frontmatter{"description": "a long description here"},
		},
		{
			name: "crlf and bom",
			in:   "\ufeff---\r\nname: demo\r\n---\r\nbody\r\n",
			want: Frontmatter{"name": "demo"}, wantBody: "body\n",
		},
		{name: "empty header", in: "---\n---\nbody", want: Frontmatter{}, wantBody: "body"},
		{name: "header only", in: "---\nname: x\n---", want: Frontmatter{"name": "x"}},
		{name: "missing opening", in: "name: x\n", wantErr: true},
		{name: "missing closing", in: "---\nname: x\n", wantErr: true},
		{name: "duplicate key", in: "---\nname: a\nname: b\n---\n", wantErr: true},
		{name: "tab indentation", in: "---\nmetadata:\n\tk: v\n---\n", wantErr: true},
		{name: "anchor", in: "---\nname: &a x\n---\n", wantErr: true},
		{name: "flow mapping", in: "---\nmetadata: {a: b}\n---\n", wantErr: true},
		{name: "deep nesting", in: "---\nmetadata:\n  a:\n    b: c\n---\n", wantErr: true},
		{name: "missing space after colon", in: "---\nname:x\n---\n", wantErr: true},
		{name: "unterminated quote", in: "---\nname: \"x\n---\n", wantErr: true},
		{name: "nested list in flow", in: "---\nx: [a, [b]]\n---\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, err := ParseSkillFile([]byte(tt.in))
			if tt.wantErr {
				if !errors.Is(err, ErrFrontmatter) {
					t.Fatalf("err = %v, want ErrFrontmatter", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fm, tt.want) {
				t.Errorf("frontmatter = %#v, want %#v", fm, tt.want)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}
