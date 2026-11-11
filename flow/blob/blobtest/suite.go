// Package blobtest is the conformance suite every blob store must pass. Run
// exercises the whole contract (content addressing, deduplication, tenant
// confinement, the size limit, digest verification, hostile keys, references
// and concurrent writers) so a backend is held to the same promises whichever
// medium it writes to.
//
// The confinement subtests are the ones that matter most. A blob store holds
// files uploaded by one customer and is asked for them by digest, and a
// digest is not a secret, so "another tenant asked for it" must be
// indistinguishable from "it does not exist" on every method.
package blobtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/blob"
)

// Factory returns a fresh, empty store. Each call must be isolated from every
// other.
type Factory func(t *testing.T) blob.Store

// Run holds a store to the Store contract.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	ctx := context.Background()

	put := func(t *testing.T, s blob.Store, tenant, content string) blob.Info {
		t.Helper()
		info, err := s.Put(ctx, tenant, strings.NewReader(content), blob.PutOptions{})
		if err != nil {
			t.Fatalf("Put(%q): %v", tenant, err)
		}
		return info
	}
	read := func(t *testing.T, s blob.Store, tenant string, d blob.Digest) string {
		t.Helper()
		rc, _, err := s.Get(ctx, tenant, d)
		if err != nil {
			t.Fatalf("Get(%q, %s): %v", tenant, d, err)
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		return string(b)
	}
	absent := func(t *testing.T, s blob.Store, tenant string, d blob.Digest) {
		t.Helper()
		if _, err := s.Stat(ctx, tenant, d); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat(%q, %s) = %v, want ErrNotFound", tenant, d, err)
		}
		if rc, _, err := s.Get(ctx, tenant, d); !errors.Is(err, blob.ErrNotFound) {
			if rc != nil {
				_ = rc.Close()
			}
			t.Errorf("Get(%q, %s) = %v, want ErrNotFound", tenant, d, err)
		}
	}

	t.Run("the key is the SHA-256 of the content and the content round-trips", func(t *testing.T) {
		s := newStore(t)
		for _, content := range []string{"hello", "", strings.Repeat("x", 1<<20), "\x00\xff binary \x00"} {
			info := put(t, s, "acme", content)
			if want := blob.Sum([]byte(content)); info.Digest != want {
				t.Errorf("digest = %s, want %s", info.Digest, want)
			}
			if info.Size != int64(len(content)) {
				t.Errorf("size = %d, want %d", info.Size, len(content))
			}
			if got := read(t, s, "acme", info.Digest); got != content {
				t.Errorf("content of %d bytes did not round-trip", len(content))
			}
			st, err := s.Stat(ctx, "acme", info.Digest)
			if err != nil || st != info {
				t.Errorf("Stat = %+v, %v, want %+v", st, err, info)
			}
		}
	})

	t.Run("the same content put twice is one blob with one stable reference", func(t *testing.T) {
		s := newStore(t)
		a := put(t, s, "acme", "same bytes")
		b := put(t, s, "acme", "same bytes")
		if a != b {
			t.Fatalf("second Put = %+v, want %+v", b, a)
		}
		if err := s.Delete(ctx, "acme", a.Digest); err != nil {
			t.Fatal(err)
		}
		// One delete removes it: there was never a second copy to outlive it.
		absent(t, s, "acme", a.Digest)
	})

	t.Run("a tenant that knows another tenant's digest cannot read, stat, delete or reference it", func(t *testing.T) {
		s := newStore(t)
		info := put(t, s, "acme", "acme's contract")
		absent(t, s, "globex", info.Digest)
		if err := s.Delete(ctx, "globex", info.Digest); err != nil {
			t.Fatalf("Delete by another tenant: %v", err)
		}
		if got := read(t, s, "acme", info.Digest); got != "acme's contract" {
			t.Fatal("another tenant's Delete removed the owner's blob")
		}
		if _, err := s.PutRef(ctx, "globex", "k", info.Digest); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("PutRef to another tenant's blob = %v, want ErrNotFound", err)
		}
		if _, err := s.PutRef(ctx, "acme", "k", info.Digest); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Ref(ctx, "globex", "k"); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Ref of another tenant's key = %v, want ErrNotFound", err)
		}
	})

	t.Run("two tenants holding identical content are independent", func(t *testing.T) {
		s := newStore(t)
		a := put(t, s, "acme", "shared logo")
		b := put(t, s, "globex", "shared logo")
		if a.Digest != b.Digest {
			t.Fatal("identical content hashed differently")
		}
		if err := s.Delete(ctx, "acme", a.Digest); err != nil {
			t.Fatal(err)
		}
		absent(t, s, "acme", a.Digest)
		if got := read(t, s, "globex", b.Digest); got != "shared logo" {
			t.Fatal("deleting one tenant's copy removed the other's")
		}
	})

	t.Run("hostile tenant names are ordinary isolated scopes", func(t *testing.T) {
		s := newStore(t)
		tenants := []string{"../acme", "..", "acme/../globex", "/etc", "a\x00b", "acme ", `c:\x`, strings.Repeat("t", 4096), "acme"}
		digests := make(map[string]blob.Digest)
		for _, tenant := range tenants {
			digests[tenant] = put(t, s, tenant, "owned by "+tenant).Digest
		}
		for _, owner := range tenants {
			if got := read(t, s, owner, digests[owner]); got != "owned by "+owner {
				t.Errorf("tenant %q read %q", owner, got)
			}
			for _, other := range tenants {
				if other != owner {
					absent(t, s, other, digests[owner])
				}
			}
		}
	})

	t.Run("content over the limit is refused and leaves nothing behind", func(t *testing.T) {
		s := newStore(t)
		content := strings.Repeat("a", 101)
		_, err := s.Put(ctx, "acme", strings.NewReader(content), blob.PutOptions{MaxBytes: 100})
		if !errors.Is(err, blob.ErrTooLarge) {
			t.Fatalf("Put over limit = %v, want ErrTooLarge", err)
		}
		absent(t, s, "acme", blob.Sum([]byte(content)))
		absent(t, s, "acme", blob.Sum([]byte(content[:100])))

		if _, err := s.Put(ctx, "acme", strings.NewReader(content[:100]), blob.PutOptions{MaxBytes: 100}); err != nil {
			t.Fatalf("Put exactly at the limit: %v", err)
		}
	})

	t.Run("an oversized upload is abandoned without reading it all", func(t *testing.T) {
		s := newStore(t)
		src := &countingReader{}
		_, err := s.Put(ctx, "acme", src, blob.PutOptions{MaxBytes: 1 << 10})
		if !errors.Is(err, blob.ErrTooLarge) {
			t.Fatalf("Put = %v, want ErrTooLarge", err)
		}
		if src.n > 1<<20 {
			t.Fatalf("read %d bytes of an endless upload against a 1KiB limit", src.n)
		}
	})

	t.Run("content that does not match the expected digest is refused and not stored", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Put(ctx, "acme", strings.NewReader("what arrived"), blob.PutOptions{Expect: blob.Sum([]byte("what was promised"))})
		if !errors.Is(err, blob.ErrDigestMismatch) {
			t.Fatalf("Put = %v, want ErrDigestMismatch", err)
		}
		absent(t, s, "acme", blob.Sum([]byte("what arrived")))

		want := blob.Sum([]byte("as promised"))
		info, err := s.Put(ctx, "acme", strings.NewReader("as promised"), blob.PutOptions{Expect: want})
		if err != nil || info.Digest != want {
			t.Fatalf("Put with matching digest = %+v, %v", info, err)
		}
	})

	t.Run("an upload that fails midway stores nothing", func(t *testing.T) {
		s := newStore(t)
		boom := errors.New("connection reset")
		_, err := s.Put(ctx, "acme", io.MultiReader(strings.NewReader("partial"), failingReader{boom}), blob.PutOptions{})
		if !errors.Is(err, boom) {
			t.Fatalf("Put = %v, want the reader's error", err)
		}
		absent(t, s, "acme", blob.Sum([]byte("partial")))
	})

	t.Run("malformed digests are rejected before storage is touched", func(t *testing.T) {
		s := newStore(t)
		valid := string(put(t, s, "acme", "x").Digest)
		bad := []string{
			"", "abc", "../../etc/passwd", valid[:63], valid + "0",
			strings.ToUpper(valid), "../" + valid[3:], valid[:62] + "/.", valid[:63] + "\x00", strings.Repeat("g", 64),
		}
		for _, d := range bad {
			if strings.ToUpper(valid) == valid && d == valid {
				continue // an all-digit digest has no uppercase form
			}
			if _, err := s.Stat(ctx, "acme", blob.Digest(d)); !errors.Is(err, blob.ErrInvalid) {
				t.Errorf("Stat(%q) = %v, want ErrInvalid", d, err)
			}
			if _, _, err := s.Get(ctx, "acme", blob.Digest(d)); !errors.Is(err, blob.ErrInvalid) {
				t.Errorf("Get(%q) = %v, want ErrInvalid", d, err)
			}
			if err := s.Delete(ctx, "acme", blob.Digest(d)); !errors.Is(err, blob.ErrInvalid) {
				t.Errorf("Delete(%q) = %v, want ErrInvalid", d, err)
			}
			if _, err := s.Put(ctx, "acme", strings.NewReader("x"), blob.PutOptions{Expect: blob.Digest(d)}); d != "" && !errors.Is(err, blob.ErrInvalid) {
				t.Errorf("Put(Expect: %q) = %v, want ErrInvalid", d, err)
			}
		}
	})

	t.Run("an empty tenant is refused on every method", func(t *testing.T) {
		s := newStore(t)
		d := blob.Sum([]byte("x"))
		if _, err := s.Put(ctx, "", strings.NewReader("x"), blob.PutOptions{}); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("Put = %v", err)
		}
		if _, _, err := s.Get(ctx, "", d); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("Get = %v", err)
		}
		if _, err := s.Stat(ctx, "", d); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("Stat = %v", err)
		}
		if err := s.Delete(ctx, "", d); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("Delete = %v", err)
		}
		if _, err := s.PutRef(ctx, "", "k", d); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("PutRef = %v", err)
		}
		if _, err := s.Ref(ctx, "", "k"); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("Ref = %v", err)
		}
		if err := s.DeleteRef(ctx, "", "k"); !errors.Is(err, blob.ErrInvalid) {
			t.Errorf("DeleteRef = %v", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		s := newStore(t)
		info := put(t, s, "acme", "gone soon")
		for i := 0; i < 2; i++ {
			if err := s.Delete(ctx, "acme", info.Digest); err != nil {
				t.Fatalf("Delete #%d: %v", i+1, err)
			}
		}
		absent(t, s, "acme", info.Digest)
		// And the content can come back under the same reference.
		if again := put(t, s, "acme", "gone soon"); again != info {
			t.Fatalf("re-Put = %+v, want %+v", again, info)
		}
	})

	t.Run("a cancelled context stops every method", func(t *testing.T) {
		s := newStore(t)
		info := put(t, s, "acme", "x")
		dead, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Put(dead, "acme", strings.NewReader("y"), blob.PutOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("Put = %v", err)
		}
		if _, _, err := s.Get(dead, "acme", info.Digest); !errors.Is(err, context.Canceled) {
			t.Errorf("Get = %v", err)
		}
		if _, err := s.Stat(dead, "acme", info.Digest); !errors.Is(err, context.Canceled) {
			t.Errorf("Stat = %v", err)
		}
		if err := s.Delete(dead, "acme", info.Digest); !errors.Is(err, context.Canceled) {
			t.Errorf("Delete = %v", err)
		}
		absent(t, s, "acme", blob.Sum([]byte("y")))
	})

	t.Run("concurrent writers of the same and different content all land intact", func(t *testing.T) {
		s := newStore(t)
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				content := "shared"
				if i%2 == 1 {
					content = strings.Repeat(string(rune('a'+i%26)), 1000+i)
				}
				info, err := s.Put(ctx, "acme", strings.NewReader(content), blob.PutOptions{})
				if err != nil {
					errs <- err
					return
				}
				rc, _, err := s.Get(ctx, "acme", info.Digest)
				if err != nil {
					errs <- err
					return
				}
				defer func() { _ = rc.Close() }()
				got, err := io.ReadAll(rc)
				if err != nil {
					errs <- err
				} else if string(got) != content {
					errs <- errors.New("content read back differs from content written")
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})

	t.Run("a stored blob does not alias the caller's buffer", func(t *testing.T) {
		s := newStore(t)
		buf := []byte("original")
		info, err := s.Put(ctx, "acme", bytes.NewReader(buf), blob.PutOptions{})
		if err != nil {
			t.Fatal(err)
		}
		copy(buf, "MUTATED!")
		if got := read(t, s, "acme", info.Digest); got != "original" {
			t.Fatalf("stored content changed to %q", got)
		}
	})

	t.Run("a reference is first writer wins so replays agree on one derived artifact", func(t *testing.T) {
		s := newStore(t)
		first, second := put(t, s, "acme", "transcript A").Digest, put(t, s, "acme", "transcript B").Digest
		if _, err := s.Ref(ctx, "acme", "derived/x"); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("Ref before PutRef = %v, want ErrNotFound", err)
		}
		if got, err := s.PutRef(ctx, "acme", "derived/x", first); err != nil || got != first {
			t.Fatalf("PutRef = %s, %v", got, err)
		}
		if got, err := s.PutRef(ctx, "acme", "derived/x", second); err != nil || got != first {
			t.Fatalf("second PutRef = %s, %v, want the first writer's %s", got, err, first)
		}
		if got, err := s.Ref(ctx, "acme", "derived/x"); err != nil || got != first {
			t.Fatalf("Ref = %s, %v", got, err)
		}
		// Keys are opaque: one that looks like a path is just another name.
		if _, err := s.Ref(ctx, "acme", "derived/x/../x"); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("a path-shaped key resolved to another key: %v", err)
		}
		if err := s.DeleteRef(ctx, "acme", "derived/x"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRef(ctx, "acme", "derived/x"); err != nil {
			t.Fatalf("second DeleteRef: %v", err)
		}
		if _, err := s.Ref(ctx, "acme", "derived/x"); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("Ref after DeleteRef = %v", err)
		}
	})

	t.Run("a reference whose blob was deleted is replaced rather than kept dangling", func(t *testing.T) {
		s := newStore(t)
		old, fresh := put(t, s, "acme", "old").Digest, put(t, s, "acme", "fresh").Digest
		if _, err := s.PutRef(ctx, "acme", "k", old); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "acme", old); err != nil {
			t.Fatal(err)
		}
		if got, err := s.PutRef(ctx, "acme", "k", fresh); err != nil || got != fresh {
			t.Fatalf("PutRef over a dangling reference = %s, %v, want %s", got, err, fresh)
		}
	})

	t.Run("racing reference writers all observe one winner", func(t *testing.T) {
		s := newStore(t)
		const n = 16
		digests := make([]blob.Digest, n)
		for i := range digests {
			digests[i] = put(t, s, "acme", strings.Repeat("v", i+1)).Digest
		}
		got := make([]blob.Digest, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				d, err := s.PutRef(ctx, "acme", "race", digests[i])
				if err != nil {
					t.Errorf("PutRef: %v", err)
				}
				got[i] = d
			}(i)
		}
		wg.Wait()
		for i := 1; i < n; i++ {
			if got[i] != got[0] {
				t.Fatalf("writers disagree on the winner: %s and %s", got[0], got[i])
			}
		}
	})

	t.Run("reference keys are bounded and required", func(t *testing.T) {
		s := newStore(t)
		d := put(t, s, "acme", "x").Digest
		for _, key := range []string{"", strings.Repeat("k", blob.MaxRefKey+1)} {
			if _, err := s.PutRef(ctx, "acme", key, d); !errors.Is(err, blob.ErrInvalid) {
				t.Errorf("PutRef(key of %d bytes) = %v, want ErrInvalid", len(key), err)
			}
		}
	})
}

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }
