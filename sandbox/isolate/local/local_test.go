package local_test

import (
	"io"
	"os"
	"testing"

	"github.com/urmzd/saige/sandbox/isolate/isolatetest"
	"github.com/urmzd/saige/sandbox/isolate/local"
)

func TestConformance(t *testing.T) {
	isolatetest.Run(t, local.Driver{Log: io.Discard}, isolatetest.Options{
		Image: "host",
		Env:   map[string]string{"PATH": os.Getenv("PATH")},
	})
}
