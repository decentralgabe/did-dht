package dht

import (
	"context"
	"crypto/sha1"
	"errors"
	"math"
	"sync"
	"time"

	k_nearest_nodes "github.com/anacrolix/dht/v2/k-nearest-nodes"
	"github.com/anacrolix/torrent/bencode"
	"github.com/sirupsen/logrus"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/bep44"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/dht/v2/traversal"
)

// Copied from https://github.com/anacrolix/dht/blob/master/exts/getput/getput.go and modified
// to return signature data and allow for context cancellations

type FullGetResult struct {
	Seq     int64
	V       bencode.Bytes
	Sig     [64]byte
	Mutable bool
}

func startGetTraversal(
	ctx context.Context, target bep44.Target, s *dht.Server, seq *int64, salt []byte,
) (
	vChan chan FullGetResult, op *traversal.Operation, err error,
) {
	vChan = make(chan FullGetResult)
	op = traversal.Start(traversal.OperationInput{
		Alpha:  15,
		Target: target,
		DoQuery: func(ctx context.Context, addr krpc.NodeAddr) traversal.QueryResult {
			queryCtx, cancel := context.WithTimeout(ctx, time.Second*8)
			defer cancel()

			res := s.Get(queryCtx, dht.NewAddr(addr.UDP()), target, seq, dht.QueryRateLimiting{})
			err := res.ToError()
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, dht.TransactionTimeout) {
				logrus.WithContext(ctx).WithError(err).Debugf("error querying %v", addr)
			}
			if r := res.Reply.R; r != nil {
				rv := r.V
				bv := rv
				if sha1.Sum(bv) == target {
					select {
					case vChan <- FullGetResult{
						V:       rv,
						Sig:     r.Sig,
						Mutable: false,
					}:
					case <-queryCtx.Done():
					}
				} else if sha1.Sum(append(r.K[:], salt...)) == target && bep44.Verify(r.K[:], salt, *r.Seq, bv, r.Sig[:]) {
					select {
					case vChan <- FullGetResult{
						Seq:     *r.Seq,
						V:       rv,
						Sig:     r.Sig,
						Mutable: true,
					}:
					case <-queryCtx.Done():
					}
				} else if rv != nil {
					logrus.WithContext(ctx).Debugf("get response item hash didn't match target: %q", rv)
				}
			}
			tqr := res.TraversalQueryResult(addr)
			tqr.ClosestData, _ = tqr.ClosestData.(string)
			if tqr.ClosestData == nil {
				tqr.ResponseFrom = nil
			}
			return tqr
		},
		NodeFilter: s.TraversalNodeFilter,
	})

	// Listen for context cancellation or stalled traversal
	go func() {
		select {
		case <-ctx.Done():
			op.Stop()
		case <-op.Stalled():
		}
	}()

	nodes, err := s.TraversalStartingNodes()
	op.AddNodes(nodes)
	return
}

func Get(
	ctx context.Context, target bep44.Target, s *dht.Server, seq *int64, salt []byte,
) (
	ret FullGetResult, stats *traversal.Stats, err error,
) {
	vChan, op, err := startGetTraversal(ctx, target, s, seq, salt)
	if err != nil {
		return
	}
	ret.Seq = math.MinInt64
	gotValue := false
receiveResults:
	select {
	case <-op.Stalled():
		if !gotValue {
			err = errors.New("value not found")
		}
	case v := <-vChan:
		logrus.WithContext(ctx).Debugf("received %#v", v)
		gotValue = true
		if !v.Mutable {
			ret = v
			break
		}
		if v.Seq >= ret.Seq {
			ret = v
		}
		goto receiveResults
	case <-ctx.Done():
		err = ctx.Err()
	}
	op.Stop()
	stats = op.Stats()
	return
}

type SeqToPut func(seq int64) bep44.Put

func Put(
	ctx context.Context, target krpc.ID, s *dht.Server, salt []byte, seqToPut SeqToPut,
) (
	stats *traversal.Stats, err error,
) {
	vChan, op, err := startGetTraversal(ctx, target, s,
		// When we do a get traversal for a put, we don't care what seq the peers have?
		nil,
		// This is duplicated with the put, but we need it to filter responses for autoSeq.
		salt)
	if err != nil {
		return
	}
	var autoSeq int64
notDone:
	select {
	case v := <-vChan:
		if v.Mutable && v.Seq > autoSeq {
			autoSeq = v.Seq
		}
		// There are more optimizations that can be done here. We can set CAS automatically, and we
		// can skip updating the sequence number if the existing content already matches (and
		// presumably republish the existing seq).
		goto notDone
	case <-op.Stalled():
	case <-ctx.Done():
		err = ctx.Err()
	}
	op.Stop()
	var wg sync.WaitGroup
	put := seqToPut(autoSeq)
	op.Closest().Range(func(elem k_nearest_nodes.Elem) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// This is enforced by startGetTraversal.
			token := elem.Data.(string)
			res := s.Put(ctx, dht.NewAddr(elem.Addr.UDP()), put, token, dht.QueryRateLimiting{})
			err = res.ToError()
			if err != nil {
				logrus.WithContext(ctx).WithError(err).Warnf("error putting to %v [token=%q]", elem.Addr, token)
			} else {
				logrus.WithContext(ctx).WithError(err).Debugf("put to %v [token=%q]", elem.Addr, token)
			}
		}()
	})
	wg.Wait()
	stats = op.Stats()
	return
}
