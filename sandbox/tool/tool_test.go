package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeProvider struct {
	tools []Tool
	err   error
}

func (p fakeProvider) Kind() Kind             { return KindCLI }
func (p fakeProvider) Tools() ([]Tool, error) { return p.tools, p.err }

func nop(name string) Tool {
	return Func(name, func(context.Context, Runtime, []byte) ([]byte, error) { return nil, nil })
}

func TestRegisterProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterProvider(fakeProvider{tools: []Tool{nop("a"), nop("b")}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Names(), ","); got != "a,b" {
		t.Fatalf("names = %q", got)
	}
}

func TestRegisterProviderAllOrNothing(t *testing.T) {
	cases := map[string]fakeProvider{
		"collides with registry": {tools: []Tool{nop("c"), nop("a")}},
		"collides with itself":   {tools: []Tool{nop("c"), nop("c")}},
		"build error":            {err: errors.New("boom")},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(nop("a")); err != nil {
				t.Fatal(err)
			}
			if err := r.RegisterProvider(p); err == nil {
				t.Fatal("want error")
			}
			if _, ok := r.Get("c"); ok {
				t.Fatal("partial registration leaked tool c")
			}
		})
	}
}
