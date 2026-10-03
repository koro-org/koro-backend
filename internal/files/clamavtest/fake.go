// Package clamavtest helps testing the ClamAV adapter: Fake is an
// in-process clamd speaking the subset of the protocol the adapter uses
// (IDSESSION, INSTREAM, PING, END), and with the integration build tag
// Main/Start run a real clamd container with a tiny signature database.
package clamavtest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// EICAR is the standard anti-virus test file (68 bytes). It is assembled
// at run time so the source file itself does not trip a local scanner.
var EICAR = []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$` + `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)

// FakeSignature is what Fake reports for content holding EICAR.
const FakeSignature = "Eicar-Test-Signature"

// Fake is an in-process clamd for unit tests. Its zero configuration
// answers "stream: OK", or "stream: Eicar-Test-Signature FOUND" when the
// stream contains EICAR.
type Fake struct {
	// Addr is host:port of the listener.
	Addr string
	// MaxBytes mimics StreamMaxLength (0 → no limit): a longer stream gets
	// "INSTREAM size limit exceeded. ERROR" and the connection is closed.
	MaxBytes int64
	// Stall, when set, is called before the n-th INSTREAM (1-based) is
	// answered; returning true makes the fake hang until the client gives
	// up (a timeout).
	Stall func(n int) bool
	// Reply, when set, replaces the verdict of the n-th INSTREAM.
	Reply func(n int, content []byte) string

	ln      net.Listener
	scans   atomic.Int64
	conns   atomic.Int64
	mu      sync.Mutex
	open    map[net.Conn]bool
	stop    chan struct{}
	stopped sync.Once
}

// NewFake starts a fake clamd on 127.0.0.1; configure it before the first
// scan. It stops when the test ends.
func NewFake(t testing.TB) *Fake {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{Addr: ln.Addr().String(), ln: ln, open: map[net.Conn]bool{}, stop: make(chan struct{})}
	go f.serve()
	t.Cleanup(f.Close)
	return f
}

// Scans is the number of INSTREAM commands received.
func (f *Fake) Scans() int { return int(f.scans.Load()) }

// Conns is the number of connections accepted.
func (f *Fake) Conns() int { return int(f.conns.Load()) }

// DropConnections closes every open connection (clamd closing idle
// sessions, or a restart).
func (f *Fake) DropConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.open {
		_ = c.Close()
		delete(f.open, c)
	}
}

// Close stops the listener and drops every connection.
func (f *Fake) Close() {
	f.stopped.Do(func() {
		close(f.stop)
		_ = f.ln.Close()
		f.DropConnections()
	})
}

func (f *Fake) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		f.mu.Lock()
		f.open[c] = true
		f.mu.Unlock()
		go f.handle(c)
	}
}

func (f *Fake) handle(c net.Conn) {
	defer func() {
		f.mu.Lock()
		delete(f.open, c)
		f.mu.Unlock()
		_ = c.Close()
	}()
	r := bufio.NewReader(c)
	session, id := false, 0
	reply := func(s string) error {
		if session {
			s = fmt.Sprintf("%d: %s", id, s)
		}
		_, err := io.WriteString(c, s+"\x00")
		return err
	}
	for {
		cmd, err := r.ReadString(0)
		if err != nil {
			return
		}
		switch strings.TrimSuffix(cmd, "\x00") {
		case "zIDSESSION":
			session = true
			continue
		case "zEND":
			return
		case "zPING":
			id++
			if reply("PONG") != nil || !session {
				return
			}
		case "zINSTREAM":
			id++
			n := int(f.scans.Add(1))
			content, err := f.readStream(r)
			if errors.Is(err, errTooLong) {
				_ = reply("INSTREAM size limit exceeded. ERROR")
				return
			}
			if err != nil {
				return
			}
			if f.Stall != nil && f.Stall(n) {
				select { // hang until the client closes or the test ends
				case <-f.stop:
				case <-closed(c):
				}
				return
			}
			answer := "stream: OK"
			if bytes.Contains(content, EICAR) {
				answer = "stream: " + FakeSignature + " FOUND"
			}
			if f.Reply != nil {
				answer = f.Reply(n, content)
			}
			if reply(answer) != nil || !session {
				return
			}
		default:
			_ = reply("UNKNOWN COMMAND")
			return
		}
	}
}

var errTooLong = errors.New("stream too long")

func (f *Fake) readStream(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			return buf.Bytes(), nil
		}
		if f.MaxBytes > 0 && int64(buf.Len())+int64(n) > f.MaxBytes {
			return nil, errTooLong
		}
		if _, err := io.CopyN(&buf, r, int64(n)); err != nil {
			return nil, err
		}
	}
}

// closed returns a channel closed once the peer closes c (a read fails).
func closed(c net.Conn) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		b := make([]byte, 1)
		for {
			_ = c.SetReadDeadline(time.Time{})
			if _, err := c.Read(b); err != nil {
				return
			}
		}
	}()
	return ch
}
