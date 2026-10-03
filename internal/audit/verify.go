package audit

import (
	"bytes"
	"context"
	"math"

	"koro/internal/platform/civil"
)

// verifyBatch is the number of chained rows read per query.
const verifyBatch = 1000

// Verification problems (VerifyResult.Problem).
const (
	problemGap       = "gap"       // a chain_seq is missing: a row was deleted
	problemPrevHash  = "prev_hash" // prev_hash differs from the previous row_hash: a row was inserted or replaced
	problemRowHash   = "row_hash"  // the row no longer hashes to row_hash: a column was edited
	problemRoot      = "root"      // the chain differs from a signed daily root: the tail was rewritten
	problemSignature = "signature" // a daily root's signature does not verify
)

// verifyRange recomputes the chain that covers the days from..to
// (Europe/Istanbul). It starts at the newest daily root before from (its
// signed hash is the anchor; the chain start when there is none) and ends
// at the root of to, or at the end of the chain when to has no root yet.
// Every daily root of the range and the anchor must carry a valid
// signature and equal the row_hash at its last_chain_seq.
//
// The first finding stops the check: OK=false with FirstBadChainSeq and
// Problem. Errors are store or signer failures, not findings.
func (s *service) verifyRange(ctx context.Context, from, to civil.Date) (VerifyResult, error) {
	res := VerifyResult{OK: true}
	bad := func(seq int64, problem string) (VerifyResult, error) {
		res.OK, res.FirstBadChainSeq, res.Problem = false, &seq, problem
		return res, nil
	}

	anchor, hasAnchor, err := s.d.Store.RootBefore(ctx, from)
	if err != nil {
		return res, err
	}
	roots, err := s.d.Store.Roots(ctx, from, to)
	if err != nil {
		return res, err
	}
	signed := roots
	if hasAnchor {
		signed = append([]DailyRoot{anchor}, roots...)
	}
	if len(signed) > 0 && s.d.Signer == nil {
		return res, ErrSignerNotConfigured
	}
	for _, r := range signed {
		ok, err := s.d.Signer.Verify(ctx, r.KeyID, rootMessage(r.Day, r.LastChainSeq, r.RootHash), r.Signature)
		if err != nil {
			return res, err
		}
		if !ok || len(r.RootHash) != HashSize {
			return bad(r.LastChainSeq, problemSignature)
		}
	}

	prev := Link{Hash: zeroHash}
	if hasAnchor {
		prev = Link{Seq: anchor.LastChainSeq, Hash: anchor.RootHash}
	}
	end := int64(math.MaxInt64)
	if n := len(roots); n > 0 && roots[n-1].Day == to {
		end = roots[n-1].LastChainSeq
	}
	rootsAt := map[int64][]DailyRoot{}
	for _, r := range roots {
		switch {
		case r.LastChainSeq < prev.Seq:
			return bad(r.LastChainSeq, problemRoot) // a root behind its predecessor
		case r.LastChainSeq == prev.Seq && !bytes.Equal(r.RootHash, prev.Hash):
			return bad(r.LastChainSeq, problemRoot) // a quiet day must repeat the previous head
		}
		rootsAt[r.LastChainSeq] = append(rootsAt[r.LastChainSeq], r)
	}

	for prev.Seq < end {
		rows, err := s.d.Store.Chained(ctx, prev.Seq, verifyBatch)
		if err != nil {
			return res, err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seq := *r.ChainSeq
			if seq > end {
				break
			}
			if seq != prev.Seq+1 {
				return bad(prev.Seq+1, problemGap)
			}
			if !bytes.Equal(r.PrevHash, prev.Hash) {
				return bad(seq, problemPrevHash)
			}
			h, err := RowHash(prev.Hash, r)
			if err != nil {
				return res, err
			}
			if !bytes.Equal(h, r.RowHash) {
				return bad(seq, problemRowHash)
			}
			for _, root := range rootsAt[seq] {
				if !bytes.Equal(root.RootHash, r.RowHash) {
					return bad(seq, problemRoot)
				}
			}
			prev = Link{Seq: seq, Hash: r.RowHash}
			res.CheckedRows++
		}
		if len(rows) < verifyBatch {
			break
		}
	}
	// Every root of the range must have been reached: rows missing at the
	// end of a signed day are a deleted tail.
	for _, r := range roots {
		if r.LastChainSeq > prev.Seq {
			return bad(prev.Seq+1, problemGap)
		}
	}
	return res, nil
}
