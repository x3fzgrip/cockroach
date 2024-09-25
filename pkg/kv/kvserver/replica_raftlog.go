// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvserver

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/logstore"
	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
)

// replicaLogStorage implements the raft.LogStorage interface.
//
// All methods require r.mu held for writes or reads. The specific requirements
// are noted in a method's comment, or can be found in assertions at the
// beginning of the implementation methods.
//
// The method names do not follow our "Locked" naming conventions, due to being
// an implementation of an interface from a different package, but in most cases
// they delegate to a method that does follow the convention.
//
// TODO(pav-kv): make it a proper type, and integrate with the logstore package.
type replicaLogStorage Replica

// Entries implements the raft.LogStorage interface.
//
// NB: maxBytes is advisory, and this method returns at least one entry (unless
// there are none in the requested interval), even if its size exceeds maxBytes.
// Sideloaded entries count towards maxBytes with their payloads inlined.
//
// Entries can return log entries that are not yet durable / synced in storage.
//
// Requires that r.mu is held for writing.
// TODO(pav-kv): make it possible to call with only raftMu held.
func (r *replicaLogStorage) Entries(lo, hi uint64, maxBytes uint64) ([]raftpb.Entry, error) {
	entries, err := r.entriesLocked(
		kvpb.RaftIndex(lo), kvpb.RaftIndex(hi), maxBytes)
	if err != nil {
		r.reportRaftStorageError(err)
	}
	return entries, err
}

// entriesLocked implements the Entries() call.
func (r *replicaLogStorage) entriesLocked(
	lo, hi kvpb.RaftIndex, maxBytes uint64,
) ([]raftpb.Entry, error) {
	// The call is always initiated by RawNode, under r.mu. Need it locked for
	// writes, for r.mu.stateLoader.
	//
	// TODO(pav-kv): we have a large class of cases when we would rather only hold
	// raftMu while reading the entries. The r.mu lock should be narrow.
	r.mu.AssertHeld()
	// Writes to the storage engine and the sideloaded storage are made under
	// raftMu only. Since we are holding r.mu, but may or may not be holding
	// raftMu, this read could be racing with a write.
	//
	// Such races are prevented at a higher level, in RawNode. Raft never reads at
	// a log index for which there is at least one in-flight entry (possibly
	// multiple, issued at different leader terms) to storage. It always reads
	// "stable" entries.
	//
	// NB: without this guarantee, there would be a concern with the sideloaded
	// storage: it doesn't provide a consistent snapshot to the reader, unlike the
	// storage engine. Its Put method writes / syncs a file sequentially, so a
	// racing reader would be able to read partial entries.
	//
	// TODO(pav-kv): we need better safety guardrails here. The log storage type
	// can remember the readable bounds, and assert that reads do not cross them.
	// TODO(pav-kv): r.raftMu.bytesAccount is broken - can't rely on raftMu here.
	entries, _, loadedSize, err := logstore.LoadEntries(
		r.AnnotateCtx(context.TODO()),
		r.mu.stateLoader.StateLoader, r.store.TODOEngine(), r.RangeID,
		r.store.raftEntryCache, r.raftMu.sideloaded, lo, hi, maxBytes,
		&r.raftMu.bytesAccount,
	)
	r.store.metrics.RaftStorageReadBytes.Inc(int64(loadedSize))
	return entries, err
}

// raftEntriesLocked implements the Entries() call.
func (r *Replica) raftEntriesLocked(
	lo, hi kvpb.RaftIndex, maxBytes uint64,
) ([]raftpb.Entry, error) {
	return (*replicaLogStorage)(r).entriesLocked(lo, hi, maxBytes)
}

// Term implements the raft.LogStorage interface.
// Requires that r.mu is held for writing.
func (r *replicaLogStorage) Term(i uint64) (uint64, error) {
	term, err := r.termLocked(kvpb.RaftIndex(i))
	if err != nil {
		r.reportRaftStorageError(err)
	}
	return uint64(term), err
}

// termLocked implements the Term() call.
func (r *replicaLogStorage) termLocked(i kvpb.RaftIndex) (kvpb.RaftTerm, error) {
	// TODO(pav-kv): make it possible to read with only raftMu held.
	r.mu.AssertHeld()
	// TODO(nvanbenschoten): should we set r.mu.lastTermNotDurable when
	//   r.mu.lastIndexNotDurable == i && r.mu.lastTermNotDurable == invalidLastTerm?
	// TODO(pav-kv): we should rather always remember the last entry term, and
	// remove invalidLastTerm special case.
	if r.mu.lastIndexNotDurable == i && r.mu.lastTermNotDurable != invalidLastTerm {
		return r.mu.lastTermNotDurable, nil
	}
	return logstore.LoadTerm(r.AnnotateCtx(context.TODO()),
		r.mu.stateLoader.StateLoader, r.store.TODOEngine(), r.RangeID,
		r.store.raftEntryCache, i,
	)
}

// raftTermLocked implements the Term() call.
func (r *Replica) raftTermLocked(i kvpb.RaftIndex) (kvpb.RaftTerm, error) {
	return (*replicaLogStorage)(r).termLocked(i)
}

// GetTerm returns the term of the entry at the given index in the raft log.
// Requires that r.mu is not held.
func (r *Replica) GetTerm(i kvpb.RaftIndex) (kvpb.RaftTerm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.raftTermLocked(i)
}

// LastIndex implements the raft.LogStorage interface.
// Requires that r.mu is held for reading.
func (r *replicaLogStorage) LastIndex() uint64 {
	return uint64((*Replica)(r).raftLastIndexRLocked())
}

// raftLastIndexRLocked implements the LastIndex() call.
func (r *Replica) raftLastIndexRLocked() kvpb.RaftIndex {
	return r.mu.lastIndexNotDurable
}

// GetLastIndex returns the index of the last entry in the raft log.
// Requires that r.mu is not held.
func (r *Replica) GetLastIndex() kvpb.RaftIndex {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.raftLastIndexRLocked()
}

// FirstIndex implements the raft.LogStorage interface.
// Requires that r.mu is held for reading.
func (r *replicaLogStorage) FirstIndex() uint64 {
	return uint64((*Replica)(r).raftFirstIndexRLocked())
}

// raftFirstIndexRLocked implements the FirstIndex() call.
func (r *Replica) raftFirstIndexRLocked() kvpb.RaftIndex {
	// TruncatedState is guaranteed to be non-nil.
	return r.mu.state.TruncatedState.Index + 1
}

// GetFirstIndex returns the index of the first entry in the raft log.
// Requires that r.mu is not held.
func (r *Replica) GetFirstIndex() kvpb.RaftIndex {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.raftFirstIndexRLocked()
}

// LogSnapshot returns an immutable point-in-time snapshot of the log storage.
// Requires that r.raftMu is held for writing, and r.mu for reading.
func (r *replicaLogStorage) LogSnapshot() raft.LogStorageSnapshot {
	r.raftMu.AssertHeld()
	r.mu.AssertRHeld()
	// TODO(pav-kv): return a wrapper which, in all methods, checks that the log
	// storage hasn't been written to. A more relaxed version of it should assert
	// that only the relevant part of the log hasn't been overwritten, e.g. a new
	// term leader hasn't appended a log slice that truncated the log, or the log
	// hasn't been wiped.
	//
	// This would require auditing and integrating with the write paths. Today,
	// this type implements only reads, and writes are in various places like the
	// logstore.LogStore type, or the code in the split handler which creates an
	// empty range state.
	//
	// We don't need a fully fledged Pebble snapshot here. For our purposes, we
	// can also make sure that raftMu is held for the entire period of using the
	// LogSnapshot - this should guarantee its immutability.
	return r
}
