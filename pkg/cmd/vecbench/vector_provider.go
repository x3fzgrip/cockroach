// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/cspann"
	"github.com/cockroachdb/cockroach/pkg/util/vector"
)

// IndexMetrics are interesting metrics about the current state of the vector
// index.
type IndexMetric struct {
	// Name of the metric.
	Name string
	// Value of the metric.
	Value float64
}

// VectorProvider abstracts the operations needed for vector storage and
// retrieval. This allows different implementations (in-memory, SQL-based, etc.)
// to provide the functionality needed by vecbench.
type VectorProvider interface {
	// Close is called once the provider is no longer needed, and gives the
	// provider a chance to do any needed cleanup.
	Close()

	// Load pulls vectors from persistent storage if they were previously saved
	// there (i.e. to a file for the in-memory provider or a database table for
	// the SQL provider). It returns false if vectors cannot be loaded, in which
	// case the caller is expected to insert them via calls to InsertVectors.
	Load(ctx context.Context) (bool, error)

	// Save ensures that all vectors are made persistent so they can be loaded
	// the next time the provider is created.
	Save(ctx context.Context) error

	// Clear deletes all vectors in the provider, including any persistent state.
	Clear(ctx context.Context) error

	// InsertVectors inserts a set of vectors into the provider, each uniquely
	// identified by a key.
	InsertVectors(ctx context.Context, keys []cspann.KeyBytes, vectors vector.Set) error

	// Search searches for vectors similar to the query vector. It returns the
	// keys of the most similar vectors. If supported, stats are recorded in
	// "stats" for this search.
	Search(
		ctx context.Context, vec vector.T, maxResults int, beamSize int, stats *cspann.SearchStats,
	) ([]cspann.KeyBytes, error)

	// GetMetrics returns interesting metrics for the vector index. Each provider
	// can have different metrics.
	GetMetrics() ([]IndexMetric, error)

	// FormatStats gets formatted statistics for the C-SPANN index.
	FormatStats() string
}
