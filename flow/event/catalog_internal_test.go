package event

import "testing"

func TestRegisterPanics(t *testing.T) {
	ok := Entry{Type: TypeText, New: func() Body { return new(Text) }, Render: func(Envelope) (string, error) { return "", nil }}
	for name, e := range map[string]Entry{
		"nil render":   {Type: ok.Type, New: ok.New},
		"nil new":      {Type: ok.Type, Render: ok.Render},
		"bad type":     {Type: "Text", New: ok.New, Render: ok.Render},
		"wrong body":   {Type: TypeThinking, New: ok.New, Render: ok.Render},
		"value body":   {Type: TypeText, New: func() Body { return Text{} }, Render: ok.Render},
		"duplicate ok": ok,
	} {
		c := &catalog{}
		if name == "duplicate ok" {
			c.register(e)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: register did not panic", name)
				}
			}()
			c.register(e)
		}()
	}
}
