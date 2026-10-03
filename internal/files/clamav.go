// The ClamAV scanner of the upload pipeline (BE-048; 05 §14.7 "ClamAV";
// FILES_SCANNER=clamav). It lives in package files because the packages of
// a module may not import each other (internal/arch). It streams an object
// to clamd over TCP with the INSTREAM command and maps the answer to a
// ScanVerdict:
//
//	stream: OK           → clean
//	stream: <sig> FOUND  → infected (Signature = <sig>)
//	anything else        → error (no verdict)
//
// Fail closed: a timeout, an unreachable clamd, an ERROR reply (size limit,
// engine failure) or a malformed answer is an error, never a clean verdict.
// files keeps such a file pending and scans it again later.
//
// Connection pool: every connection opens a clamd session (IDSESSION), so
// one TCP connection serves many INSTREAM commands; each reply carries the
// command's id and is checked against it. At most MaxConns scans run at
// once (the pool size); an idle session is reused if it was used less than
// IdleTimeout ago (below clamd's own IdleTimeout, 30 s by default) and is
// closed otherwise. A reused session that fails before clamd answered is
// retried once on a fresh connection (clamd may have closed it).
//
// Limits: an object larger than MaxBytes (clamd's StreamMaxLength) is not
// sent; every scan has a deadline (Timeout, or the caller's earlier one).

package files

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"koro/internal/platform/clock"
)

// Defaults of Config.
const (
	DefaultClamAVTimeout     = 60 * time.Second
	DefaultClamAVDialTimeout = 5 * time.Second
	DefaultClamAVMaxConns    = 4
	DefaultClamAVMaxBytes    = 25 << 20 // clamd StreamMaxLength default (25M)
	DefaultClamAVIdleTimeout = 20 * time.Second

	// clamChunkSize is the size of one INSTREAM chunk.
	clamChunkSize = 64 << 10
	// clamMaxReply bounds a clamd reply line.
	clamMaxReply = 4 << 10
	// clamMaxSignature bounds the signature name kept in a verdict.
	clamMaxSignature = 128
)

// Errors. Every one of them means "no verdict".
var (
	// ErrClamAVTooLarge: the object exceeds MaxBytes (or clamd's StreamMaxLength).
	ErrClamAVTooLarge = errors.New("files: clamav: object exceeds the stream size limit")
	// ErrClamAVScanFailed: clamd answered with an error.
	ErrClamAVScanFailed = errors.New("files: clamav: clamd reported an error")
	// ErrClamAVProtocol: the answer is not one clamd gives to the command.
	ErrClamAVProtocol = errors.New("files: clamav: unexpected answer from clamd")
	// ErrClamAVShortObject: the object's length differs from the verified size.
	ErrClamAVShortObject = errors.New("files: clamav: object length differs from its size")
)

// ClamAVConfig configures the ClamAV scanner.
type ClamAVConfig struct {
	// Addr is clamd's TCPSocket, host:port.
	Addr string
	// Timeout bounds one scan (connect, stream, verdict); 0 → DefaultClamAVTimeout.
	Timeout time.Duration
	// DialTimeout bounds connecting; 0 → DefaultClamAVDialTimeout (at most Timeout).
	DialTimeout time.Duration
	// MaxConns is the pool size: concurrent scans and idle sessions kept;
	// 0 → DefaultClamAVMaxConns.
	MaxConns int
	// MaxBytes is the largest object sent (clamd StreamMaxLength); 0 →
	// DefaultClamAVMaxBytes.
	MaxBytes int64
	// IdleTimeout is how long an idle session may be reused; 0 →
	// DefaultClamAVIdleTimeout. Keep it below clamd's IdleTimeout.
	IdleTimeout time.Duration
	// Clock sets the connection deadlines. Required.
	Clock clock.Clock
	// Dial opens a connection; nil → net.Dialer (tests inject failures).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// ClamAV is the ClamAV scanner. It implements Scanner and is safe for
// concurrent use.
type ClamAV struct {
	cfg ClamAVConfig
	sem chan struct{} // one token per running scan

	mu   sync.Mutex
	idle []*clamSession // most recently used last

	dials atomic.Int64
}

var _ Scanner = (*ClamAV)(nil)

// NewClamAV returns a scanner for cfg. It does not connect: clamd may start
// after the process (Ping reports its state).
func NewClamAV(cfg ClamAVConfig) (*ClamAV, error) {
	if cfg.Clock == nil {
		return nil, errors.New("files: clamav: Config.Clock is required")
	}
	if host, port, err := net.SplitHostPort(cfg.Addr); err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("files: clamav: address %q is not host:port", cfg.Addr)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultClamAVTimeout
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultClamAVDialTimeout
	}
	cfg.DialTimeout = min(cfg.DialTimeout, cfg.Timeout)
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = DefaultClamAVMaxConns
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultClamAVMaxBytes
	}
	if cfg.MaxBytes > 1<<32-1 { // INSTREAM chunk lengths are uint32; clamd's limit is below 4 GiB
		return nil, fmt.Errorf("files: clamav: MaxBytes %d is too large", cfg.MaxBytes)
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultClamAVIdleTimeout
	}
	if cfg.Dial == nil {
		d := &net.Dialer{}
		cfg.Dial = d.DialContext
	}
	return &ClamAV{cfg: cfg, sem: make(chan struct{}, cfg.MaxConns)}, nil
}

// Name implements Scanner.
func (*ClamAV) Name() string { return "clamav" }

// Real implements Scanner: ClamAV inspects the content.
func (*ClamAV) Real() bool { return true }

// MaxBytes is the largest object the scanner accepts (files checks it
// against the upload limits at start).
func (s *ClamAV) MaxBytes() int64 { return s.cfg.MaxBytes }

// Dials reports how many connections the scanner has opened (pool metric
// and tests).
func (s *ClamAV) Dials() int64 { return s.dials.Load() }

// Scan implements Scanner.
func (s *ClamAV) Scan(ctx context.Context, obj ScanObject) (ScanVerdict, error) {
	if obj.Size < 0 || obj.Size > s.cfg.MaxBytes {
		return ScanVerdict{}, fmt.Errorf("%w: %d bytes, limit %d", ErrClamAVTooLarge, obj.Size, s.cfg.MaxBytes)
	}
	if obj.Open == nil {
		return ScanVerdict{}, errors.New("files: clamav: ScanObject.Open is nil")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	select { // pool: at most MaxConns scans at once
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ScanVerdict{}, fmt.Errorf("files: clamav: waiting for a connection: %w", ctx.Err())
	}
	defer func() { <-s.sem }()

	for attempt := 0; ; attempt++ {
		sess, reused, err := s.take(ctx)
		if err != nil {
			return ScanVerdict{}, err
		}
		v, answered, err := s.instream(ctx, sess, obj)
		if err == nil {
			s.put(sess)
			return v, nil
		}
		sess.close()
		// clamd closes idle or broken sessions on its side: a reused one
		// that failed before any answer gets one fresh connection.
		if reused && !answered && attempt == 0 && ctx.Err() == nil {
			continue
		}
		return ScanVerdict{}, err
	}
}

// Ping checks that clamd answers (the optional /readyz check). It uses its
// own short connection, not the pool.
func (s *ClamAV) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()
	conn, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := s.deadline(ctx, conn)
	defer stop()
	if _, err := io.WriteString(conn, "zPING\x00"); err != nil {
		return fmt.Errorf("files: clamav: ping: %w", err)
	}
	reply, err := clamReadReply(bufio.NewReader(conn))
	if err != nil {
		return fmt.Errorf("files: clamav: ping: %w", err)
	}
	if reply != "PONG" {
		return fmt.Errorf("%w: ping answered %q", ErrClamAVProtocol, clamClip(reply))
	}
	return nil
}

// Close closes the idle sessions (process shutdown). Scans still running
// close their own session when they finish.
func (s *ClamAV) Close() {
	s.mu.Lock()
	idle := s.idle
	s.idle = nil
	s.mu.Unlock()
	for _, ss := range idle {
		s.end(ss)
	}
}

// session is one clamd connection in IDSESSION mode.
type clamSession struct {
	conn net.Conn
	r    *bufio.Reader
	next int       // id clamd gives the next command (1 for the first)
	used time.Time // last successful command
}

func (ss *clamSession) close() { _ = ss.conn.Close() }

// end leaves the session politely (END) and closes the connection.
func (s *ClamAV) end(ss *clamSession) {
	_ = ss.conn.SetWriteDeadline(s.cfg.Clock.Now().Add(time.Second))
	_, _ = io.WriteString(ss.conn, "zEND\x00")
	ss.close()
}

// take returns an idle session young enough to reuse, or a new one.
func (s *ClamAV) take(ctx context.Context) (*clamSession, bool, error) {
	now := s.cfg.Clock.Now()
	var stale []*clamSession
	var got *clamSession
	s.mu.Lock()
	for len(s.idle) > 0 && got == nil {
		ss := s.idle[len(s.idle)-1]
		s.idle = s.idle[:len(s.idle)-1]
		if now.Sub(ss.used) < s.cfg.IdleTimeout {
			got = ss
		} else {
			stale = append(stale, ss)
		}
	}
	s.mu.Unlock()
	for _, ss := range stale {
		ss.close()
	}
	if got != nil {
		return got, true, nil
	}
	conn, err := s.dial(ctx)
	if err != nil {
		return nil, false, err
	}
	ss := &clamSession{conn: conn, r: bufio.NewReaderSize(conn, clamMaxReply), next: 1}
	stop := s.deadline(ctx, conn)
	_, err = io.WriteString(conn, "zIDSESSION\x00")
	stop()
	if err != nil {
		ss.close()
		return nil, false, fmt.Errorf("files: clamav: open session: %w", err)
	}
	return ss, false, nil
}

// put returns a healthy session to the pool (or closes it when full).
func (s *ClamAV) put(ss *clamSession) {
	ss.used = s.cfg.Clock.Now()
	s.mu.Lock()
	if len(s.idle) < s.cfg.MaxConns {
		s.idle = append(s.idle, ss)
		ss = nil
	}
	s.mu.Unlock()
	if ss != nil {
		s.end(ss)
	}
}

func (s *ClamAV) dial(ctx context.Context) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()
	conn, err := s.cfg.Dial(dctx, "tcp", s.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("files: clamav: connect to clamd: %w", err)
	}
	s.dials.Add(1)
	return conn, nil
}

// deadline applies the deadline of ctx to conn and interrupts blocked I/O
// when ctx is cancelled. The returned func detaches the cancellation and
// clears the deadline.
func (s *ClamAV) deadline(ctx context.Context, conn net.Conn) func() {
	d, ok := ctx.Deadline()
	if !ok {
		d = s.cfg.Clock.Now().Add(s.cfg.Timeout)
	}
	_ = conn.SetDeadline(d)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	return func() {
		stop()
		_ = conn.SetDeadline(time.Time{})
	}
}

// instream sends obj with INSTREAM on ss and reads the verdict. answered
// reports whether clamd sent any reply (a stale session never does).
func (s *ClamAV) instream(ctx context.Context, ss *clamSession, obj ScanObject) (v ScanVerdict, answered bool, err error) {
	stop := s.deadline(ctx, ss.conn)
	defer stop()
	id := ss.next
	ss.next++

	sendErr := s.send(ctx, ss.conn, obj)
	if errors.Is(sendErr, ErrClamAVShortObject) || errors.Is(sendErr, errClamRead) {
		return v, false, sendErr // our side failed; the session is dropped
	}
	// clamd answers even when it stops reading (size limit): read the reply
	// before reporting a write error.
	reply, err := clamReadReply(ss.r)
	if err != nil {
		if sendErr != nil {
			return v, false, fmt.Errorf("files: clamav: stream: %w", sendErr)
		}
		return v, false, fmt.Errorf("files: clamav: read verdict: %w", err)
	}
	prefix := strconv.Itoa(id) + ": "
	if !strings.HasPrefix(reply, prefix) {
		return v, true, fmt.Errorf("%w: reply %q to command %d", ErrClamAVProtocol, clamClip(reply), id)
	}
	v, err = clamVerdict(strings.TrimPrefix(reply, prefix))
	if err == nil && sendErr != nil { // a verdict on a stream we could not finish is not trusted
		err = fmt.Errorf("files: clamav: stream: %w", sendErr)
	}
	return v, true, err
}

// errClamRead marks a failure to read the object (storage), not clamd's.
var errClamRead = errors.New("files: clamav: read object")

// send writes the INSTREAM command, the object in chunks and the
// terminating zero-length chunk.
func (s *ClamAV) send(ctx context.Context, conn net.Conn, obj ScanObject) error {
	rc, err := obj.Open(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errClamRead, err)
	}
	defer rc.Close()
	w := bufio.NewWriterSize(conn, clamChunkSize+4)
	if _, err := w.WriteString("zINSTREAM\x00"); err != nil {
		return err
	}
	buf := make([]byte, clamChunkSize)
	var hdr [4]byte
	var sent int64
	for {
		n, rerr := io.ReadFull(rc, buf)
		if n > 0 {
			sent += int64(n)
			if sent > obj.Size {
				return fmt.Errorf("%w: more than %d bytes", ErrClamAVShortObject, obj.Size)
			}
			binary.BigEndian.PutUint32(hdr[:], uint32(n)) //nolint:gosec // G115: n ≤ clamChunkSize
			if _, err := w.Write(hdr[:]); err != nil {
				return err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			return fmt.Errorf("%w: %w", errClamRead, rerr)
		}
	}
	if sent != obj.Size {
		return fmt.Errorf("%w: %d of %d bytes", ErrClamAVShortObject, sent, obj.Size)
	}
	binary.BigEndian.PutUint32(hdr[:], 0)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	return w.Flush()
}

// clamReadReply reads one NUL-terminated reply (z-prefixed commands).
func clamReadReply(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice(0)
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return "", fmt.Errorf("%w: reply longer than %d bytes", ErrClamAVProtocol, clamMaxReply)
	case err != nil:
		return "", err
	}
	return strings.TrimSpace(string(line[:len(line)-1])), nil
}

// verdict maps the reply of INSTREAM (without the session id).
func clamVerdict(reply string) (ScanVerdict, error) {
	switch {
	case reply == "stream: OK":
		return ScanVerdict{Status: ScanClean}, nil
	case strings.HasPrefix(reply, "stream: ") && strings.HasSuffix(reply, " FOUND"):
		sig := strings.TrimSuffix(strings.TrimPrefix(reply, "stream: "), " FOUND")
		return ScanVerdict{Status: ScanInfected, Signature: clamSignature(sig)}, nil
	case strings.Contains(reply, "size limit exceeded"):
		return ScanVerdict{}, fmt.Errorf("%w (clamd StreamMaxLength)", ErrClamAVTooLarge)
	case strings.HasSuffix(reply, " ERROR"):
		return ScanVerdict{}, fmt.Errorf("%w: %q", ErrClamAVScanFailed, clamClip(reply))
	}
	return ScanVerdict{}, fmt.Errorf("%w: %q", ErrClamAVProtocol, clamClip(reply))
}

// signature keeps a log- and audit-safe signature name: clamd prints the
// name of the matching rule; anything outside its alphabet is replaced.
func clamSignature(s string) string {
	b := []byte(strings.TrimSpace(s))
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("._-:/+", c) >= 0:
		default:
			b[i] = '_'
		}
	}
	if len(b) > clamMaxSignature {
		b = b[:clamMaxSignature]
	}
	if len(b) == 0 {
		return "unknown"
	}
	return string(b)
}

// clip shortens a reply for an error message.
func clamClip(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
