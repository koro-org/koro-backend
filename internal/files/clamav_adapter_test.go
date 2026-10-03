package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/files"
	"koro/internal/files/clamavtest"
	"koro/internal/platform/clock"
)

func scanObject(b []byte) files.ScanObject {
	return files.ScanObject{
		FileID: uuid.NewV7(), Purpose: files.PurposeCV, MIME: files.MIMEPDF, Size: int64(len(b)),
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
	}
}

func newClamAV(t *testing.T, cfg files.ClamAVConfig) *files.ClamAV {
	t.Helper()
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	s, err := files.NewClamAV(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

var clamDoc = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\n%%EOF\n")

// TestClamAV_CleanFile: a clean object is streamed in full and comes back
// clean; consecutive scans share one pooled clamd session.
func TestClamAV_CleanFile(t *testing.T) {
	t.Parallel()
	clamd := clamavtest.NewFake(t)
	var mu sync.Mutex
	var seen [][]byte
	clamd.Reply = func(_ int, content []byte) string {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, bytes.Clone(content))
		return "stream: OK"
	}
	s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
	big := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320 KB: several INSTREAM chunks
	for _, b := range [][]byte{clamDoc, big, clamDoc} {
		v, err := s.Scan(t.Context(), scanObject(b))
		if err != nil || v.Status != files.ScanClean || v.Signature != "" {
			t.Fatalf("Scan = %+v, %v; want clean", v, err)
		}
	}
	if !bytes.Equal(seen[0], clamDoc) || !bytes.Equal(seen[1], big) {
		t.Fatal("clamd did not receive the object byte for byte")
	}
	if s.Dials() != 1 || clamd.Conns() != 1 || clamd.Scans() != 3 {
		t.Fatalf("dials %d, connections %d, scans %d: want one pooled session for 3 scans", s.Dials(), clamd.Conns(), clamd.Scans())
	}
	if s.Name() != "clamav" || !s.Real() || s.MaxBytes() != files.DefaultClamAVMaxBytes {
		t.Fatalf("scanner identity %s %v %d", s.Name(), s.Real(), s.MaxBytes())
	}
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestClamAV_Verdicts maps every clamd answer; anything but OK or FOUND is
// an error, never a clean verdict (fail closed).
func TestClamAV_Verdicts(t *testing.T) {
	t.Parallel()
	t.Run("eicar is infected", func(t *testing.T) {
		t.Parallel()
		s := newClamAV(t, files.ClamAVConfig{Addr: clamavtest.NewFake(t).Addr})
		v, err := s.Scan(t.Context(), scanObject(clamavtest.EICAR))
		if err != nil || v.Status != files.ScanInfected || v.Signature != clamavtest.FakeSignature {
			t.Fatalf("Scan(EICAR) = %+v, %v", v, err)
		}
	})
	t.Run("signature is sanitised", func(t *testing.T) {
		t.Parallel()
		clamd := clamavtest.NewFake(t)
		clamd.Reply = func(int, []byte) string { return "stream: Evil\tName<script>ü FOUND" }
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
		v, err := s.Scan(t.Context(), scanObject(clamDoc))
		if err != nil || v.Status != files.ScanInfected || v.Signature != "Evil_Name_script___" {
			t.Fatalf("Scan = %+v, %v", v, err)
		}
	})
	for _, tc := range []struct {
		name, reply string
		want        error
	}{
		{"engine error", "stream: Can't allocate memory ERROR", files.ErrClamAVScanFailed},
		{"size limit answer", "INSTREAM size limit exceeded. ERROR", files.ErrClamAVTooLarge},
		{"garbage", "hello", files.ErrClamAVProtocol},
		{"empty", "", files.ErrClamAVProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clamd := clamavtest.NewFake(t)
			clamd.Reply = func(int, []byte) string { return tc.reply }
			s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
			v, err := s.Scan(t.Context(), scanObject(clamDoc))
			if !errors.Is(err, tc.want) || v.Status != "" {
				t.Fatalf("Scan = %+v, %v; want no verdict and %v", v, err, tc.want)
			}
		})
	}
	t.Run("clamd stream limit", func(t *testing.T) {
		t.Parallel()
		clamd := clamavtest.NewFake(t)
		clamd.MaxBytes = 100 << 10
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
		v, err := s.Scan(t.Context(), scanObject(bytes.Repeat([]byte{'a'}, 1<<20)))
		if !errors.Is(err, files.ErrClamAVTooLarge) || v.Status != "" {
			t.Fatalf("Scan(over clamd limit) = %+v, %v", v, err)
		}
	})
	t.Run("object over MaxBytes is not sent", func(t *testing.T) {
		t.Parallel()
		clamd := clamavtest.NewFake(t)
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr, MaxBytes: 1024})
		_, err := s.Scan(t.Context(), scanObject(bytes.Repeat([]byte{'a'}, 2048)))
		if !errors.Is(err, files.ErrClamAVTooLarge) || clamd.Conns() != 0 {
			t.Fatalf("Scan = %v with %d connections", err, clamd.Conns())
		}
	})
	t.Run("object shorter or longer than verified", func(t *testing.T) {
		t.Parallel()
		clamd := clamavtest.NewFake(t)
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
		for _, size := range []int64{int64(len(clamDoc)) + 10, int64(len(clamDoc)) - 10} {
			obj := scanObject(clamDoc)
			obj.Size = size
			if _, err := s.Scan(t.Context(), obj); !errors.Is(err, files.ErrClamAVShortObject) {
				t.Fatalf("Scan(size %d of %d) = %v", size, len(clamDoc), err)
			}
		}
	})
	t.Run("object read error", func(t *testing.T) {
		t.Parallel()
		s := newClamAV(t, files.ClamAVConfig{Addr: clamavtest.NewFake(t).Addr})
		obj := scanObject(clamDoc)
		obj.Open = func(context.Context) (io.ReadCloser, error) { return nil, errors.New("s3: timeout") }
		if _, err := s.Scan(t.Context(), obj); err == nil || !strings.Contains(err.Error(), "s3: timeout") {
			t.Fatalf("Scan = %v", err)
		}
	})
	t.Run("clamd unreachable", func(t *testing.T) {
		t.Parallel()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := newClamAV(t, files.ClamAVConfig{Addr: addr, Timeout: 2 * time.Second})
		if v, err := s.Scan(t.Context(), scanObject(clamDoc)); err == nil || v.Status != "" {
			t.Fatalf("Scan without clamd = %+v, %v", v, err)
		}
		if err := s.Ping(t.Context()); err == nil {
			t.Fatal("Ping without clamd succeeded")
		}
	})
}

// TestClamAV_Timeout: a clamd that does not answer is abandoned at the
// deadline (Timeout or the caller's), with no verdict.
func TestClamAV_Timeout(t *testing.T) {
	t.Parallel()
	clamd := clamavtest.NewFake(t)
	clamd.Stall = func(int) bool { return true }
	s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr, Timeout: 200 * time.Millisecond})
	done := make(chan error, 1)
	go func() {
		_, err := s.Scan(t.Context(), scanObject(clamDoc))
		done <- err
	}()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("Scan = %v, want a timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scan ignored its timeout")
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() { _, err := s.Scan(ctx, scanObject(clamDoc)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled scan returned a verdict")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scan ignored the cancellation")
	}
}

// TestClamAV_PoolReusesAndRecovers: idle sessions are reused while young,
// replaced when clamd dropped them (one retry on a fresh connection) or
// when they idled too long; concurrency never exceeds MaxConns.
func TestClamAV_PoolReusesAndRecovers(t *testing.T) {
	t.Parallel()
	t.Run("dropped session is replaced", func(t *testing.T) {
		t.Parallel()
		clamd := clamavtest.NewFake(t)
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr})
		if _, err := s.Scan(t.Context(), scanObject(clamDoc)); err != nil {
			t.Fatal(err)
		}
		clamd.DropConnections() // clamd IdleTimeout or restart
		v, err := s.Scan(t.Context(), scanObject(clamDoc))
		if err != nil || v.Status != files.ScanClean || s.Dials() != 2 {
			t.Fatalf("after a dropped session: %+v, %v, %d dials", v, err, s.Dials())
		}
	})
	t.Run("idle session expires", func(t *testing.T) {
		t.Parallel()
		clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
		clamd := clamavtest.NewFake(t)
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr, Clock: clk, IdleTimeout: 10 * time.Second})
		for range 2 {
			if _, err := s.Scan(t.Context(), scanObject(clamDoc)); err != nil {
				t.Fatal(err)
			}
		}
		clk.Advance(11 * time.Second)
		if _, err := s.Scan(t.Context(), scanObject(clamDoc)); err != nil {
			t.Fatal(err)
		}
		if s.Dials() != 2 {
			t.Fatalf("%d dials, want 2 (reuse, then a fresh session after the idle timeout)", s.Dials())
		}
	})
	t.Run("at most MaxConns connections", func(t *testing.T) {
		t.Parallel()
		clamd := clamavtest.NewFake(t)
		s := newClamAV(t, files.ClamAVConfig{Addr: clamd.Addr, MaxConns: 2})
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for range 12 {
			wg.Go(func() {
				v, err := s.Scan(t.Context(), scanObject(bytes.Repeat(clamDoc, 2000)))
				if err == nil && v.Status != files.ScanClean {
					err = errors.New("not clean")
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if clamd.Conns() > 2 || clamd.Scans() != 12 {
			t.Fatalf("%d connections for %d scans, want at most 2", clamd.Conns(), clamd.Scans())
		}
	})
}

func TestClamAV_New_Validates(t *testing.T) {
	t.Parallel()
	for _, cfg := range []files.ClamAVConfig{
		{Addr: "127.0.0.1:3310"},                                         // no clock
		{Addr: "clamd", Clock: clock.Real()},                             // no port
		{Addr: ":3310", Clock: clock.Real()},                             // no host
		{Addr: "127.0.0.1:3310", Clock: clock.Real(), MaxBytes: 5 << 30}, // over uint32
	} {
		if _, err := files.NewClamAV(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
}
