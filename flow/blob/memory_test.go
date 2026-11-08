package blob_test

import (
	"testing"

	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/blob/blobtest"
)

func TestMemorySatisfiesTheStoreContract(t *testing.T) {
	blobtest.Run(t, func(*testing.T) blob.Store { return blob.NewMemory() })
}

func TestDigestHasOneCanonicalSpelling(t *testing.T) {
	valid := string(blob.Sum([]byte("x")))
	if _, err := blob.ParseDigest(valid); err != nil {
		t.Fatalf("ParseDigest(valid) = %v", err)
	}
	for _, s := range []string{"", "sha256:" + valid, valid[:10], valid + "a", "../" + valid[3:], "A" + valid[1:], " " + valid[1:]} {
		if _, err := blob.ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) accepted a non-canonical digest", s)
		}
	}
}
