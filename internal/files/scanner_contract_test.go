package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"koro/internal/files"
	"koro/internal/files/clamavtest"
)

// scannerFixture is what the Scanner contract suite needs from the adapter's
// environment.
type scannerFixture struct {
	// Infected is content the scanner must report infected (a real scanner).
	Infected []byte
	// Down makes the scanner unavailable for good (the last step); nil skips it.
	Down func()
}

// scannerContract is the contract suite of files.Scanner (BE-113; the rules
// of the Scanner doc comment): NoopScanner and the ClamAV adapter (fake
// clamd here, real clamd with -tags integration) pass it. A new scanner is a
// new adapter file, one line in cmd/koro/wire_files.go scanners and a
// Test<Adapter>_ContractSuite that runs this suite. The steps run in order.
func scannerContract(t *testing.T, s files.Scanner, fx scannerFixture) {
	t.Helper()
	ctx := t.Context()
	if name := s.Name(); name == "" || name != strings.ToLower(name) || s.Name() != name {
		t.Fatalf("Name() = %q: a stable lower-case name is logged and audited", name)
	}
	real := s.Real()
	// opened counts the reads of one object.
	object := func(b []byte, opened *atomic.Int64) files.ScanObject {
		obj := scanObject(b)
		obj.Open = func(context.Context) (io.ReadCloser, error) {
			opened.Add(1)
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		return obj
	}

	t.Run("clean content", func(t *testing.T) {
		var opened atomic.Int64
		v, err := s.Scan(ctx, object(clamDoc, &opened))
		switch {
		case err != nil:
			t.Fatalf("Scan(clean) error = %v", err)
		case real && (v.Status != files.ScanClean || opened.Load() == 0):
			t.Fatalf("real scanner: verdict %+v after %d reads, want clean after reading the content", v, opened.Load())
		case !real && (v.Status != files.ScanSkipped || opened.Load() != 0):
			t.Fatalf("not a real scanner: verdict %+v after %d reads, want skipped without reading (lazy Open)", v, opened.Load())
		case v.Signature != "":
			t.Fatalf("clean verdict carries a signature %q", v.Signature)
		}
	})

	t.Run("infected content", func(t *testing.T) {
		var opened atomic.Int64
		v, err := s.Scan(ctx, object(fx.Infected, &opened))
		if err != nil {
			t.Fatalf("Scan(infected) error = %v", err)
		}
		if !real {
			if v.Status != files.ScanSkipped {
				t.Fatalf("not a real scanner: verdict %+v, want skipped", v)
			}
			return
		}
		if v.Status != files.ScanInfected || v.Signature == "" {
			t.Fatalf("verdict %+v, want infected with a signature", v)
		}
		if len(fx.Infected) > 8 && strings.Contains(v.Signature, string(fx.Infected[:8])) {
			t.Fatalf("signature %q contains file content", v.Signature)
		}
	})

	t.Run("a read failure is no verdict, never clean", func(t *testing.T) {
		obj := scanObject(clamDoc)
		obj.Open = func(context.Context) (io.ReadCloser, error) { return nil, errors.New("storage: open object: down") }
		v, err := s.Scan(ctx, obj)
		if real && err == nil {
			t.Fatalf("real scanner: Scan(unreadable) = %+v, want an error (the file stays pending)", v)
		}
		if err == nil && v.Status != files.ScanSkipped {
			t.Fatalf("Scan(unreadable) = %+v without error, want skipped or an error", v)
		}
	})

	t.Run("a cancelled context is no verdict, never clean", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		var opened atomic.Int64
		v, err := s.Scan(cctx, object(clamDoc, &opened))
		if err == nil && v.Status == files.ScanClean {
			t.Fatalf("Scan(cancelled) = %+v, want an error or skipped", v)
		}
	})

	t.Run("concurrent scans are independent", func(t *testing.T) {
		var wg sync.WaitGroup
		got := make([]files.ScanVerdict, 8)
		errs := make([]error, len(got))
		for i := range got {
			wg.Go(func() {
				b := clamDoc
				if i%2 == 1 {
					b = fx.Infected
				}
				var opened atomic.Int64
				got[i], errs[i] = s.Scan(ctx, object(b, &opened))
			})
		}
		wg.Wait()
		for i, v := range got {
			want := files.ScanSkipped
			if real {
				want = files.ScanClean
				if i%2 == 1 {
					want = files.ScanInfected
				}
			}
			if errs[i] != nil || v.Status != want {
				t.Fatalf("scan %d = %+v, %v; want %s", i, v, errs[i], want)
			}
		}
	})

	if fx.Down != nil {
		t.Run("an unavailable scanner is no verdict, never clean", func(t *testing.T) {
			fx.Down()
			var opened atomic.Int64
			if v, err := s.Scan(ctx, object(clamDoc, &opened)); err == nil {
				t.Fatalf("Scan while down = %+v, want an error", v)
			}
		})
	}
}

func TestNoopScanner_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	scannerContract(t, files.NoopScanner{}, scannerFixture{Infected: clamavtest.EICAR})
}

func TestClamAV_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	clamd := clamavtest.NewFake(t)
	s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
	scannerContract(t, s, scannerFixture{Infected: clamavtest.EICAR, Down: clamd.Close})
}
