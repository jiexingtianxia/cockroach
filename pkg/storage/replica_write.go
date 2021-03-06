// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/closedts/ctpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/spanlatch"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/storage/storagepb"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/pkg/errors"
)

// executeWriteBatch is the entry point for client requests which may mutate the
// range's replicated state. Requests taking this path are evaluated and ultimately
// serialized through Raft, but pass through additional machinery whose goal is
// to allow commands which commute to be proposed in parallel. The naive
// alternative, submitting requests to Raft one after another, paying massive
// latency, is only taken for commands whose effects may overlap.
//
// Concretely,
//
// - Latches for the keys affected by the command are acquired (i.e.
//   tracked as in-flight mutations).
// - In doing so, we wait until no overlapping mutations are in flight.
// - The timestamp cache is checked to determine if the command's affected keys
//   were accessed with a timestamp exceeding that of the command; if so, the
//   command's timestamp is incremented accordingly.
// - A RaftCommand is constructed. If proposer-evaluated KV is active,
//   the request is evaluated and the Result is placed in the
//   RaftCommand. If not, the request itself is added to the command.
// - The proposal is inserted into the Replica's in-flight proposals map,
//   a lease index is assigned to it, and it is submitted to Raft, returning
//   a channel.
// - The result of the Raft proposal is read from the channel and the command
//   registered with the timestamp cache, its latches are released, and
//   its result (which could be an error) is returned to the client.
//
// Returns exactly one of a response, an error or re-evaluation reason.
//
// NB: changing BatchRequest to a pointer here would have to be done cautiously
// as this method makes the assumption that it operates on a shallow copy (see
// call to applyTimestampCache).
func (r *Replica) executeWriteBatch(
	ctx context.Context, ba *roachpb.BatchRequest, spans *spanset.SpanSet, lg *spanlatch.Guard,
) (br *roachpb.BatchResponse, pErr *roachpb.Error) {
	startTime := timeutil.Now()

	// Guarantee we release the provided latches if we never make it to
	// passing responsibility to evalAndPropose. This is wrapped to delay
	// pErr evaluation to its value when returning.
	ec := endCmds{repl: r, lg: lg}
	defer func() {
		// No-op if we move ec into evalAndPropose.
		ec.done(ctx, ba, br, pErr)
	}()

	// Determine the lease under which to evaluate the write.
	var lease roachpb.Lease
	var status storagepb.LeaseStatus
	// For lease commands, use the provided previous lease for verification.
	if ba.IsSingleSkipLeaseCheckRequest() {
		lease = ba.GetPrevLeaseForLeaseRequest()
	} else {
		// Other write commands require that this replica has the range
		// lease.
		if status, pErr = r.redirectOnOrAcquireLease(ctx); pErr != nil {
			return nil, pErr
		}
		lease = status.Lease
	}
	r.limitTxnMaxTimestamp(ctx, ba, status)

	// Verify that the batch can be executed.
	// NB: we only need to check that the request is in the Range's key bounds
	// at proposal time, not at application time, because the spanlatch manager
	// will synchronize all requests (notably EndTxn with SplitTrigger) that may
	// cause this condition to change.
	if err := r.checkExecutionCanProceed(ba, ec.lg, &status); err != nil {
		return nil, roachpb.NewError(err)
	}

	minTS, untrack := r.store.cfg.ClosedTimestamp.Tracker.Track(ctx)
	defer untrack(ctx, 0, 0, 0) // covers all error returns below

	// Examine the timestamp cache for preceding commands which require this
	// command to move its timestamp forward. Or, in the case of a transactional
	// write, the txn timestamp and possible write-too-old bool.
	if bumped := r.applyTimestampCache(ctx, ba, minTS); bumped {
		// If we bump the transaction's timestamp, we must absolutely
		// tell the client in a response transaction (for otherwise it
		// doesn't know about the incremented timestamp). Response
		// transactions are set far away from this code, but at the time
		// of writing, they always seem to be set. Since that is a
		// likely target of future micro-optimization, this assertion is
		// meant to protect against future correctness anomalies.
		defer func() {
			if br != nil && ba.Txn != nil && br.Txn == nil {
				log.Fatalf(ctx, "assertion failed: transaction updated by "+
					"timestamp cache, but transaction returned in response; "+
					"updated timestamp would have been lost (recovered): "+
					"%s in batch %s", ba.Txn, ba,
				)
			}
		}()
	}
	log.Event(ctx, "applied timestamp cache")

	// Checking the context just before proposing can help avoid ambiguous errors.
	if err := ctx.Err(); err != nil {
		log.VEventf(ctx, 2, "%s before proposing: %s", err, ba.Summary())
		return nil, roachpb.NewError(errors.Wrap(err, "aborted before proposing"))
	}

	// After the command is proposed to Raft, invoking endCmds.done is the
	// responsibility of Raft, so move the endCmds into evalAndPropose.
	ch, abandon, maxLeaseIndex, pErr := r.evalAndPropose(ctx, &lease, ba, spans, ec.move())
	if pErr != nil {
		if maxLeaseIndex != 0 {
			log.Fatalf(
				ctx, "unexpected max lease index %d assigned to failed proposal: %s, error %s",
				maxLeaseIndex, ba, pErr,
			)
		}
		return nil, pErr
	}
	// A max lease index of zero is returned when no proposal was made or a lease was proposed.
	// In both cases, we don't need to communicate a MLAI. Furthermore, for lease proposals we
	// cannot communicate under the lease's epoch. Instead the code calls EmitMLAI explicitly
	// as a side effect of stepping up as leaseholder.
	if maxLeaseIndex != 0 {
		untrack(ctx, ctpb.Epoch(lease.Epoch), r.RangeID, ctpb.LAI(maxLeaseIndex))
	}

	// If the command was accepted by raft, wait for the range to apply it.
	ctxDone := ctx.Done()
	shouldQuiesce := r.store.stopper.ShouldQuiesce()
	startPropTime := timeutil.Now()
	slowTimer := timeutil.NewTimer()
	defer slowTimer.Stop()
	slowTimer.Reset(base.SlowRequestThreshold)
	// NOTE: this defer was moved from a case in the select statement to here
	// because escape analysis does a better job avoiding allocations to the
	// heap when defers are unconditional. When this was in the slowTimer select
	// case, it was causing pErr to escape.
	defer func() {
		if slowTimer.Read {
			r.store.metrics.SlowRaftRequests.Dec(1)
			log.Infof(
				ctx,
				"slow command %s finished after %.2fs with error %v",
				ba,
				timeutil.Since(startPropTime).Seconds(),
				pErr,
			)
		}
	}()

	for {
		select {
		case propResult := <-ch:
			// Semi-synchronously process any intents that need resolving here in
			// order to apply back pressure on the client which generated them. The
			// resolution is semi-synchronous in that there is a limited number of
			// outstanding asynchronous resolution tasks allowed after which
			// further calls will block.
			if len(propResult.EncounteredIntents) > 0 {
				// TODO(peter): Re-proposed and canceled (but executed) commands can
				// both leave intents to GC that don't hit this code path. No good
				// solution presents itself at the moment and such intents will be
				// resolved on reads.
				if err := r.store.intentResolver.CleanupIntentsAsync(
					ctx, propResult.EncounteredIntents, true, /* allowSync */
				); err != nil {
					log.Warning(ctx, err)
				}
			}
			if len(propResult.EndTxns) > 0 {
				if err := r.store.intentResolver.CleanupTxnIntentsAsync(
					ctx, r.RangeID, propResult.EndTxns, true, /* allowSync */
				); err != nil {
					log.Warning(ctx, err)
				}
			}
			return propResult.Reply, propResult.Err
		case <-slowTimer.C:
			slowTimer.Read = true
			r.store.metrics.SlowRaftRequests.Inc(1)
			log.Warningf(ctx, `have been waiting %.2fs for proposing command %s.
This range is likely unavailable.
Please submit this message at

  https://github.com/cockroachdb/cockroach/issues/new/choose

along with

	https://yourhost:8080/#/reports/range/%d

and the following Raft status: %+v`,
				timeutil.Since(startPropTime).Seconds(),
				ba,
				r.RangeID,
				r.RaftStatus(),
			)
		case <-ctxDone:
			// If our context was canceled, return an AmbiguousResultError,
			// which indicates to the caller that the command may have executed.
			abandon()
			log.VEventf(ctx, 2, "context cancellation after %0.1fs of attempting command %s",
				timeutil.Since(startTime).Seconds(), ba)
			return nil, roachpb.NewError(roachpb.NewAmbiguousResultError(ctx.Err().Error()))
		case <-shouldQuiesce:
			// If shutting down, return an AmbiguousResultError, which indicates
			// to the caller that the command may have executed.
			abandon()
			log.VEventf(ctx, 2, "shutdown cancellation after %0.1fs of attempting command %s",
				timeutil.Since(startTime).Seconds(), ba)
			return nil, roachpb.NewError(roachpb.NewAmbiguousResultError("server shutdown"))
		}
	}
}

// evaluateWriteBatch evaluates the supplied batch.
//
// If the batch is transactional and has all the hallmarks of a 1PC commit (i.e.
// includes all intent writes & EndTxn, and there's nothing to suggest that the
// transaction will require retry or restart), the batch's txn is stripped and
// it's executed as an atomic batch write. If the writes cannot all be completed
// at the intended timestamp, the batch's txn is restored and it's re-executed
// in full. This allows it to lay down intents and return an appropriate
// retryable error.
func (r *Replica) evaluateWriteBatch(
	ctx context.Context, idKey storagebase.CmdIDKey, ba *roachpb.BatchRequest, spans *spanset.SpanSet,
) (engine.Batch, enginepb.MVCCStats, *roachpb.BatchResponse, result.Result, *roachpb.Error) {
	ms := enginepb.MVCCStats{}

	// If the transaction has been pushed but it can commit at the higher
	// timestamp, let's evaluate the batch at the bumped timestamp. This will
	// allow it commit, and also it'll allow us to attempt the 1PC code path.
	maybeBumpReadTimestampToWriteTimestamp(ctx, ba)

	// Attempt 1PC execution, if applicable. If not transactional or there are
	// indications that the batch's txn will require retry, execute as normal.
	if isOnePhaseCommit(ba) {
		log.VEventf(ctx, 2, "attempting 1PC execution")
		arg, _ := ba.GetArg(roachpb.EndTxn)
		etArg := arg.(*roachpb.EndTxnRequest)

		if ba.Timestamp != ba.Txn.WriteTimestamp {
			log.Fatalf(ctx, "unexpected 1PC execution with diverged timestamp. %s != %s",
				ba.Timestamp, ba.Txn.WriteTimestamp)
		}

		// Try executing with transaction stripped.
		strippedBa := *ba
		strippedBa.Txn = nil
		// strippedBa is non-transactional, so DeferWriteTooOldError cannot be set.
		strippedBa.DeferWriteTooOldError = false
		strippedBa.Requests = ba.Requests[:len(ba.Requests)-1] // strip end txn req

		rec := NewReplicaEvalContext(r, spans)
		batch, br, res, pErr := r.evaluateWriteBatchWithServersideRefreshes(
			ctx, idKey, rec, &ms, &strippedBa, spans,
		)

		type onePCResult struct {
			success bool
			// pErr is set if success == false and regular transactional execution
			// should not be attempted. Conversely, if success is not set and pErr is
			// not set, then transactional execution should be attempted.
			pErr *roachpb.Error

			// The fields below are only set when success is set.
			stats enginepb.MVCCStats
			br    *roachpb.BatchResponse
			res   result.Result
		}
		synthesizeEndTxnResponse := func() onePCResult {
			if pErr != nil {
				return onePCResult{success: false}
			}
			commitTS := br.Timestamp

			// If we were pushed ...
			if ba.Timestamp != commitTS &&
				// ... and the batch can't commit at the pushed timestamp ...
				(!batcheval.CanForwardCommitTimestampWithoutRefresh(ba.Txn, etArg) ||
					batcheval.IsEndTxnExceedingDeadline(commitTS, etArg)) {
				// ... then the 1PC execution was not successful.
				return onePCResult{success: false}
			}

			// 1PC execution was successful, let's synthesize an EndTxnResponse.

			clonedTxn := ba.Txn.Clone()
			clonedTxn.Status = roachpb.COMMITTED
			// Make sure the returned txn has the actual commit
			// timestamp. This can be different if the stripped batch was
			// executed at the server's hlc now timestamp.
			clonedTxn.WriteTimestamp = br.Timestamp

			// If the end transaction is not committed, clear the batch and mark the status aborted.
			if !etArg.Commit {
				clonedTxn.Status = roachpb.ABORTED
				batch.Close()
				batch = r.store.Engine().NewBatch()
				ms = enginepb.MVCCStats{}
			} else {
				// Run commit trigger manually.
				innerResult, err := batcheval.RunCommitTrigger(ctx, rec, batch, &ms, etArg, clonedTxn)
				if err != nil {
					return onePCResult{
						success: false,
						pErr:    roachpb.NewErrorf("failed to run commit trigger: %s", err),
					}
				}
				if err := res.MergeAndDestroy(innerResult); err != nil {
					return onePCResult{
						success: false,
						pErr:    roachpb.NewError(err),
					}
				}
			}

			// Add placeholder responses for end transaction requests.
			br.Add(&roachpb.EndTxnResponse{OnePhaseCommit: true})
			br.Txn = clonedTxn
			return onePCResult{
				success: true,
				stats:   ms,
				br:      br,
				res:     res,
			}
		}
		onePCRes := synthesizeEndTxnResponse()
		if onePCRes.success {
			return batch, onePCRes.stats, onePCRes.br, onePCRes.res, nil
		}
		if onePCRes.pErr != nil {
			return batch, enginepb.MVCCStats{}, nil, result.Result{}, onePCRes.pErr
		}

		// Handle the case of a required one phase commit transaction.
		if etArg.Require1PC {
			// Make sure that there's a pErr returned.
			if pErr != nil {
				return batch, enginepb.MVCCStats{}, nil, result.Result{}, pErr
			}
			if ba.Timestamp != br.Timestamp {
				onePCRes.pErr = roachpb.NewError(
					roachpb.NewTransactionRetryError(
						roachpb.RETRY_SERIALIZABLE, "Require1PC batch pushed"))
				return batch, enginepb.MVCCStats{}, nil, result.Result{}, pErr
			}
			log.Fatal(ctx, "unreachable")
		}

		ms = enginepb.MVCCStats{}

		batch.Close()
		if log.ExpensiveLogEnabled(ctx, 2) {
			log.VEventf(ctx, 2,
				"1PC execution failed, reverting to regular execution for batch. pErr: %v", pErr.String())
		}
	}

	rec := NewReplicaEvalContext(r, spans)
	batch, br, res, pErr := r.evaluateWriteBatchWithServersideRefreshes(
		ctx, idKey, rec, &ms, ba, spans)
	return batch, ms, br, res, pErr
}

// evaluateWriteBatchWithServersideRefreshes invokes evaluateBatch and retries
// at a higher timestamp in the event of some retriable errors if allowed by the
// batch/txn.
func (r *Replica) evaluateWriteBatchWithServersideRefreshes(
	ctx context.Context,
	idKey storagebase.CmdIDKey,
	rec batcheval.EvalContext,
	ms *enginepb.MVCCStats,
	ba *roachpb.BatchRequest,
	spans *spanset.SpanSet,
) (batch engine.Batch, br *roachpb.BatchResponse, res result.Result, pErr *roachpb.Error) {
	goldenMS := *ms
	for retries := 0; ; retries++ {
		if retries > 0 {
			log.VEventf(ctx, 2, "server-side retry of batch")
		}
		if batch != nil {
			*ms = goldenMS
			batch.Close()
		}
		batch = r.store.Engine().NewBatch()
		var opLogger *engine.OpLoggerBatch
		if RangefeedEnabled.Get(&r.store.cfg.Settings.SV) {
			// TODO(nvanbenschoten): once we get rid of the RangefeedEnabled
			// cluster setting we'll need a way to turn this on when any
			// replica (not just the leaseholder) wants it and off when no
			// replicas want it. This turns out to be pretty involved.
			//
			// The current plan is to:
			// - create a range-id local key that stores all replicas that are
			//   subscribed to logical operations, along with their corresponding
			//   liveness epoch.
			// - create a new command that adds or subtracts replicas from this
			//   structure. The command will be a write across the entire replica
			//   span so that it is serialized with all writes.
			// - each replica will add itself to this set when it first needs
			//   logical ops. It will then wait until it sees the replicated command
			//   that added itself pop out through Raft so that it knows all
			//   commands that are missing logical ops are gone.
			// - It will then proceed as normal, relying on the logical ops to
			//   always be included on the raft commands. When its no longer
			//   needs logical ops, it will remove itself from the set.
			// - The leaseholder will have a new queue to detect registered
			//   replicas that are no longer live and remove them from the
			//   set to prevent "leaking" subscriptions.
			// - The condition here to add logical logging will be:
			//     if len(replicaState.logicalOpsSubs) > 0 { ... }
			//
			// An alternative to this is the reduce the cost of the including
			// the logical op log to a negligible amount such that it can be
			// included on all raft commands, regardless of whether any replica
			// has a rangefeed running or not.
			//
			// Another alternative is to make the setting table/zone-scoped
			// instead of a fine-grained per-replica state.
			opLogger = engine.NewOpLoggerBatch(batch)
			batch = opLogger
		}
		if util.RaceEnabled {
			// During writes we may encounter a versioned value newer than the request
			// timestamp, and may have to retry at a higher timestamp. This is still
			// safe as we're only ever writing at timestamps higher than the timestamp
			// any write latch would be declared at. But because of this, we don't
			// assert on access timestamps using spanset.NewBatchAt.
			batch = spanset.NewBatch(batch, spans)
		}

		br, res, pErr = evaluateBatch(ctx, idKey, batch, rec, ms, ba, false /* readOnly */)
		if pErr == nil {
			if opLogger != nil {
				res.LogicalOpLog = &storagepb.LogicalOpLog{
					Ops: opLogger.LogicalOps(),
				}
			}
		}
		// If we can retry, set a higher batch timestamp and continue.
		// Allow one retry only; a non-txn batch containing overlapping
		// spans will always experience WriteTooOldError.
		if pErr == nil || retries > 0 || !canDoServersideRetry(ctx, pErr, ba) {
			break
		}
	}
	return
}

// canDoServersideRetry looks at the error produced by evaluating ba and decides
// if it's possible to retry the batch evaluation at a higher timestamp.
// Retrying is sometimes possible in case of some retriable errors which ask for
// higher timestamps : for transactional requests, retrying is possible if the
// transaction had not performed any prior reads that need refreshing.
//
// If true is returned, ba and ba.Txn will have been updated with the new
// timestamp.
func canDoServersideRetry(ctx context.Context, pErr *roachpb.Error, ba *roachpb.BatchRequest) bool {
	var deadline *hlc.Timestamp
	if ba.Txn != nil {
		// Transaction requests can only be retried if there's an EndTransaction
		// telling us that there's been no prior reads in the transaction.
		etArg, ok := ba.GetArg(roachpb.EndTxn)
		if !ok {
			return false
		}
		et := etArg.(*roachpb.EndTxnRequest)
		if !batcheval.CanForwardCommitTimestampWithoutRefresh(ba.Txn, et) {
			return false
		}
		deadline = et.Deadline
	}
	var newTimestamp hlc.Timestamp
	switch tErr := pErr.GetDetail().(type) {
	case *roachpb.WriteTooOldError:
		newTimestamp = tErr.ActualTimestamp
	case *roachpb.TransactionRetryError:
		if ba.Txn == nil {
			// TODO(andrei): I don't know if TransactionRetryError is possible for
			// non-transactional batches, but some tests inject them for 1PC
			// transactions. I'm not sure how to deal with them, so let's not retry.
			return false
		}
		newTimestamp = pErr.GetTxn().WriteTimestamp
	default:
		// TODO(andrei): Handle other retriable errors too.
		return false
	}
	if deadline != nil && deadline.LessEq(newTimestamp) {
		return false
	}
	bumpBatchTimestamp(ctx, ba, newTimestamp)
	return true
}

// isOnePhaseCommit returns true iff the BatchRequest contains all writes in the
// transaction and ends with an EndTxn. One phase commits are disallowed if any
// of the following conditions are true:
// (1) the transaction has already been flagged with a write too old error
// (2) the transaction's commit timestamp has been forwarded
// (3) the transaction exceeded its deadline
// (4) the transaction is not in its first epoch and the EndTxn request does
//     not require one phase commit.
func isOnePhaseCommit(ba *roachpb.BatchRequest) bool {
	if ba.Txn == nil {
		return false
	}
	if !ba.IsCompleteTransaction() {
		return false
	}
	arg, _ := ba.GetArg(roachpb.EndTxn)
	etArg := arg.(*roachpb.EndTxnRequest)
	if retry, _, _ := batcheval.IsEndTxnTriggeringRetryError(ba.Txn, etArg); retry {
		return false
	}
	// If the transaction has already restarted at least once then it may have
	// left intents at prior epochs that need to be cleaned up during the
	// process of committing the transaction. Even if the current epoch could
	// perform a one phase commit, we don't allow it to because that could
	// prevent it from properly resolving intents from prior epochs and cause
	// it to abandon them instead.
	//
	// The exception to this rule is transactions that require a one phase
	// commit. We know that if they also required a one phase commit in past
	// epochs then they couldn't have left any intents that they now need to
	// clean up.
	return ba.Txn.Epoch == 0 || etArg.Require1PC
}
