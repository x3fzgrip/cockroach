// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package vecindex

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/internal"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/quantize"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/testutils"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/vecstore"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/num32"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/vector"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestVectorIndex(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := internal.WithWorkspace(context.Background(), &internal.Workspace{})
	state := testState{T: t, Ctx: ctx, Stopper: stop.NewStopper()}
	defer state.Stopper.Stop(ctx)

	datadriven.Walk(t, "testdata", func(t *testing.T, path string) {
		if regexp.MustCompile("/.+/").MatchString(path) {
			// Skip files that are in subdirs.
			return
		}
		if !strings.HasSuffix(path, ".ddt") {
			// Skip files that are not data-driven tests.
			return
		}
		datadriven.RunTest(t, path, func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "new-index":
				return state.NewIndex(d)

			case "format-tree":
				return state.FormatTree(d)

			case "search":
				return state.Search(d)

			case "search-for-insert":
				return state.SearchForInsert(d)

			case "search-for-delete":
				return state.SearchForDelete(d)

			case "insert":
				return state.Insert(d)

			case "delete":
				return state.Delete(d)

			case "force-split", "force-merge":
				return state.ForceSplitOrMerge(d)

			case "recall":
				return state.Recall(d)

			case "validate-tree":
				return state.ValidateTree(d)
			}

			t.Fatalf("unknown cmd: %s", d.Cmd)
			return ""
		})
	})
}

type testState struct {
	T          *testing.T
	Ctx        context.Context
	Stopper    *stop.Stopper
	Quantizer  quantize.Quantizer
	InMemStore *vecstore.InMemoryStore
	Index      *VectorIndex
	Options    VectorIndexOptions
	Features   vector.Set
}

func (s *testState) NewIndex(d *datadriven.TestData) string {
	var err error
	dims := 2
	s.Options = VectorIndexOptions{IsDeterministic: true}
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "min-partition-size":
			require.Len(s.T, arg.Vals, 1)
			s.Options.MinPartitionSize, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "max-partition-size":
			require.Len(s.T, arg.Vals, 1)
			s.Options.MaxPartitionSize, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "quality-samples":
			require.Len(s.T, arg.Vals, 1)
			s.Options.QualitySamples, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "dims":
			require.Len(s.T, arg.Vals, 1)
			dims, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "beam-size":
			require.Len(s.T, arg.Vals, 1)
			s.Options.BaseBeamSize, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
		}
	}

	s.Quantizer = quantize.NewRaBitQuantizer(dims, 42)
	s.InMemStore = vecstore.NewInMemoryStore(dims, 42)
	s.Index, err = NewVectorIndex(s.Ctx, s.InMemStore, s.Quantizer, 42, &s.Options, s.Stopper)
	require.NoError(s.T, err)

	// Suspend background fixups until ProcessFixups is explicitly called, so
	// that vector index operations can be deterministic.
	s.Index.SuspendFixups()

	// Insert initial vectors.
	return s.Insert(d)
}

func (s *testState) FormatTree(d *datadriven.TestData) string {
	txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
	defer commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

	tree, err := s.Index.Format(s.Ctx, txn, FormatOptions{PrimaryKeyStrings: true})
	require.NoError(s.T, err)
	return tree
}

func (s *testState) Search(d *datadriven.TestData) string {
	var vec vector.T
	searchSet := vecstore.SearchSet{MaxResults: 1}
	options := SearchOptions{}

	var err error
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "use-feature":
			require.Len(s.T, arg.Vals, 1)
			offset, err := strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
			vec = s.Features.At(offset)

		case "max-results":
			require.Len(s.T, arg.Vals, 1)
			searchSet.MaxResults, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "beam-size":
			require.Len(s.T, arg.Vals, 1)
			options.BaseBeamSize, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "skip-rerank":
			require.Len(s.T, arg.Vals, 0)
			options.SkipRerank = true
		}
	}

	if vec == nil {
		// Parse input as the vector to search for.
		vec = s.parseVector(d.Input)
	}

	// Search the index within a transaction.
	txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
	err = s.Index.Search(s.Ctx, txn, vec, &searchSet, options)
	require.NoError(s.T, err)
	commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

	var buf bytes.Buffer
	results := searchSet.PopResults()
	for i := range results {
		result := &results[i]
		var errorBound string
		if result.ErrorBound != 0 {
			errorBound = fmt.Sprintf("±%s ", formatFloat(result.ErrorBound, 2))
		}
		fmt.Fprintf(&buf, "%s: %s %s(centroid=%s)\n",
			string(result.ChildKey.PrimaryKey), formatFloat(result.QuerySquaredDistance, 4),
			errorBound, formatFloat(result.CentroidDistance, 2))
	}

	buf.WriteString(fmt.Sprintf("%d leaf vectors, ", searchSet.Stats.QuantizedLeafVectorCount))
	buf.WriteString(fmt.Sprintf("%d vectors, ", searchSet.Stats.QuantizedVectorCount))
	buf.WriteString(fmt.Sprintf("%d full vectors, ", searchSet.Stats.FullVectorCount))
	buf.WriteString(fmt.Sprintf("%d partitions", searchSet.Stats.PartitionCount))

	// Handle any fixups triggered by the search.
	s.Index.ProcessFixups()

	return buf.String()
}

func (s *testState) SearchForInsert(d *datadriven.TestData) string {
	var vec vector.T

	var err error
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "use-feature":
			require.Len(s.T, arg.Vals, 1)
			offset, err := strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
			vec = s.Features.At(offset)
		}
	}

	if vec == nil {
		// Parse input as the vector to search for.
		vec = s.parseVector(d.Input)
	}

	// Search the index within a transaction.
	txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
	result, err := s.Index.SearchForInsert(s.Ctx, txn, vec)
	require.NoError(s.T, err)
	commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "partition %d, centroid=", result.ChildKey.PartitionKey)

	// Un-randomize the centroid and write it to buffer.
	original := make(vector.T, len(result.Vector))
	s.Index.unRandomizeVector(result.Vector, original)
	writeVector(&buf, original, 4)

	fmt.Fprintf(&buf, ", sqdist=%s", formatFloat(result.QuerySquaredDistance, 4))
	if result.ErrorBound != 0 {
		fmt.Fprintf(&buf, "±%s", formatFloat(result.ErrorBound, 2))
	}
	buf.WriteByte('\n')

	// Handle any fixups triggered by the search.
	s.Index.ProcessFixups()

	return buf.String()
}

func (s *testState) SearchForDelete(d *datadriven.TestData) string {
	var buf bytes.Buffer

	for _, line := range strings.Split(d.Input, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		key, vec := s.parseKeyAndVector(line)

		// Search within a transaction.
		txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
		result, err := s.Index.SearchForDelete(s.Ctx, txn, vec, key)
		require.NoError(s.T, err)
		commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

		if result == nil {
			fmt.Fprintf(&buf, "%s: vector not found\n", string(key))
		} else {
			fmt.Fprintf(&buf, "%s: partition %d\n", string(key), result.ParentPartitionKey)
		}
	}

	// Handle any fixups triggered by the search.
	s.Index.ProcessFixups()

	return buf.String()
}

func (s *testState) Insert(d *datadriven.TestData) string {
	var err error
	hideTree := false
	noFixups := false
	count := 0
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "load-features":
			require.Len(s.T, arg.Vals, 1)
			count, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "hide-tree":
			require.Len(s.T, arg.Vals, 0)
			hideTree = true

		case "no-fixups":
			require.Len(s.T, arg.Vals, 0)
			noFixups = true
		}
	}

	vectors := vector.MakeSet(s.Quantizer.GetDims())
	childKeys := make([]vecstore.ChildKey, 0, count)
	if count != 0 {
		// Load features.
		s.Features = testutils.LoadFeatures(s.T, 10000)
		vectors = s.Features
		vectors.SplitAt(count)
		for i := 0; i < count; i++ {
			key := vecstore.PrimaryKey(fmt.Sprintf("vec%d", i))
			childKeys = append(childKeys, vecstore.ChildKey{PrimaryKey: key})
		}
	} else {
		// Parse vectors.
		for _, line := range strings.Split(d.Input, "\n") {
			line = strings.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			parts := strings.Split(line, ":")
			require.Len(s.T, parts, 2)

			vectors.Add(s.parseVector(parts[1]))
			key := vecstore.PrimaryKey(parts[0])
			childKeys = append(childKeys, vecstore.ChildKey{PrimaryKey: key})
		}
	}

	var wait sync.WaitGroup
	step := (s.Options.MinPartitionSize + s.Options.MaxPartitionSize) / 2
	for i := 0; i < vectors.Count; i++ {
		// Insert within the scope of a transaction.
		txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
		s.InMemStore.InsertVector(childKeys[i].PrimaryKey, vectors.At(i))
		require.NoError(s.T, s.Index.Insert(s.Ctx, txn, vectors.At(i), childKeys[i].PrimaryKey))
		commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

		if (i+1)%step == 0 && !noFixups {
			// Run synchronous fixups so that test results are deterministic.
			s.Index.ProcessFixups()
		}
	}
	wait.Wait()

	if !noFixups {
		// Handle any remaining fixups.
		s.Index.ProcessFixups()
	}

	if hideTree {
		str := fmt.Sprintf("Created index with %d vectors with %d dimensions.\n",
			vectors.Count, vectors.Dims)
		return str + s.Index.FormatStats()
	}

	return s.FormatTree(d)
}

func (s *testState) Delete(d *datadriven.TestData) string {
	notFound := false
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "not-found":
			require.Len(s.T, arg.Vals, 0)
			notFound = true
		}
	}

	for i, line := range strings.Split(d.Input, "\n") {
		line = strings.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		key, vec := s.parseKeyAndVector(line)

		// Delete within the scope of a transaction.
		txn := beginTransaction(s.Ctx, s.T, s.InMemStore)

		// If notFound=true, then simulate case where the vector is deleted in
		// the primary index, but it cannot be found in the secondary index.
		if !notFound {
			err := s.Index.Delete(s.Ctx, txn, vec, key)
			require.NoError(s.T, err)
		}
		s.InMemStore.DeleteVector(key)

		commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

		if (i+1)%s.Options.MaxPartitionSize == 0 {
			// Run synchronous fixups so that test results are deterministic.
			s.Index.ProcessFixups()
		}
	}

	// Handle any remaining fixups.
	s.Index.ProcessFixups()

	return s.FormatTree(d)
}

func (s *testState) ForceSplitOrMerge(d *datadriven.TestData) string {
	var parentPartitionKey, partitionKey vecstore.PartitionKey
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "parent-partition-key":
			require.Len(s.T, arg.Vals, 1)
			val, err := strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
			parentPartitionKey = vecstore.PartitionKey(val)

		case "partition-key":
			require.Len(s.T, arg.Vals, 1)
			val, err := strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
			partitionKey = vecstore.PartitionKey(val)
		}
	}

	if d.Cmd == "force-split" {
		s.Index.ForceSplit(s.Ctx, parentPartitionKey, partitionKey)
	} else {
		s.Index.ForceMerge(s.Ctx, parentPartitionKey, partitionKey)
	}

	// Ensure the fixup runs.
	s.Index.ProcessFixups()

	return s.FormatTree(d)
}

func (s *testState) Recall(d *datadriven.TestData) string {
	searchSet := vecstore.SearchSet{MaxResults: 1}
	options := SearchOptions{}
	numSamples := 50
	var samples []int
	seed := 42
	var err error
	for _, arg := range d.CmdArgs {
		switch arg.Key {
		case "use-feature":
			// Use single designated sample.
			require.Len(s.T, arg.Vals, 1)
			offset, err := strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
			numSamples = 1
			samples = []int{offset}

		case "samples":
			require.Len(s.T, arg.Vals, 1)
			numSamples, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "seed":
			require.Len(s.T, arg.Vals, 1)
			seed, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "topk":
			require.Len(s.T, arg.Vals, 1)
			searchSet.MaxResults, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)

		case "beam-size":
			require.Len(s.T, arg.Vals, 1)
			options.BaseBeamSize, err = strconv.Atoi(arg.Vals[0])
			require.NoError(s.T, err)
		}
	}

	data := s.InMemStore.GetAllVectors()

	// Construct list of feature offsets.
	if samples == nil {
		// Shuffle the remaining features.
		rng := rand.New(rand.NewSource(int64(seed)))
		remaining := make([]int, s.Features.Count-len(data))
		for i := range remaining {
			remaining[i] = i
		}
		rng.Shuffle(len(remaining), func(i, j int) {
			remaining[i], remaining[j] = remaining[j], remaining[i]
		})

		// Pick numSamples randomly from the remaining set
		samples = make([]int, numSamples)
		copy(samples, remaining[:numSamples])
	}

	txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
	defer commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

	// calcTruth calculates the true nearest neighbors for the query vector.
	calcTruth := func(queryVector vector.T, data []vecstore.VectorWithKey) []vecstore.PrimaryKey {
		distances := make([]float32, len(data))
		offsets := make([]int, len(data))
		for i := 0; i < len(data); i++ {
			distances[i] = num32.L2SquaredDistance(queryVector, data[i].Vector)
			offsets[i] = i
		}
		sort.SliceStable(offsets, func(i int, j int) bool {
			res := cmp.Compare(distances[offsets[i]], distances[offsets[j]])
			if res != 0 {
				return res < 0
			}
			return data[offsets[i]].Key.Compare(data[offsets[j]].Key) < 0
		})

		truth := make([]vecstore.PrimaryKey, searchSet.MaxResults)
		for i := 0; i < len(truth); i++ {
			truth[i] = data[offsets[i]].Key.PrimaryKey
		}
		return truth
	}

	// Search for sampled features.
	var sumMAP float64
	for i := range samples {
		// Calculate truth set for the vector.
		queryVector := s.Features.At(samples[i])
		truth := calcTruth(queryVector, data)

		// Calculate prediction set for the vector.
		err = s.Index.Search(s.Ctx, txn, queryVector, &searchSet, options)
		require.NoError(s.T, err)
		results := searchSet.PopResults()

		prediction := make([]vecstore.PrimaryKey, searchSet.MaxResults)
		for res := 0; res < len(results); res++ {
			prediction[res] = results[res].ChildKey.PrimaryKey
		}

		sumMAP += findMAP(prediction, truth)
	}

	recall := sumMAP / float64(numSamples) * 100
	quantizedLeafVectors := float64(searchSet.Stats.QuantizedLeafVectorCount) / float64(numSamples)
	quantizedVectors := float64(searchSet.Stats.QuantizedVectorCount) / float64(numSamples)
	fullVectors := float64(searchSet.Stats.FullVectorCount) / float64(numSamples)
	partitions := float64(searchSet.Stats.PartitionCount) / float64(numSamples)

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("%.2f%% recall@%d\n", recall, searchSet.MaxResults))
	buf.WriteString(fmt.Sprintf("%.2f leaf vectors, ", quantizedLeafVectors))
	buf.WriteString(fmt.Sprintf("%.2f vectors, ", quantizedVectors))
	buf.WriteString(fmt.Sprintf("%.2f full vectors, ", fullVectors))
	buf.WriteString(fmt.Sprintf("%.2f partitions", partitions))
	return buf.String()
}

func (s *testState) ValidateTree(d *datadriven.TestData) string {
	txn := beginTransaction(s.Ctx, s.T, s.InMemStore)
	defer commitTransaction(s.Ctx, s.T, s.InMemStore, txn)

	vectorCount := 0
	partitionKeys := []vecstore.PartitionKey{vecstore.RootKey}
	for {
		// Get all child keys for next level.
		var childKeys []vecstore.ChildKey
		for _, key := range partitionKeys {
			partition, err := txn.GetPartition(s.Ctx, key)
			require.NoError(s.T, err)
			childKeys = append(childKeys, partition.ChildKeys()...)
		}

		if len(childKeys) == 0 {
			break
		}

		// Verify full vectors exist for the level.
		refs := make([]vecstore.VectorWithKey, len(childKeys))
		for i := range childKeys {
			refs[i].Key = childKeys[i]
		}
		err := txn.GetFullVectors(s.Ctx, refs)
		require.NoError(s.T, err)
		for i := range refs {
			require.NotNil(s.T, refs[i].Vector)
		}

		// If this is not the leaf level, then process the next level.
		if childKeys[0].PrimaryKey == nil {
			partitionKeys = make([]vecstore.PartitionKey, len(childKeys))
			for i := range childKeys {
				partitionKeys[i] = childKeys[i].PartitionKey
			}
		} else {
			// This is the leaf level, so count vectors and end.
			vectorCount += len(childKeys)
			break
		}
	}

	return fmt.Sprintf("Validated index with %d vectors.\n", vectorCount)
}

// parseVector parses a vector string in this form: (1.5, 6, -4).
func (s *testState) parseVector(str string) vector.T {
	// Remove parentheses and split by commas.
	str = strings.TrimSpace(str)
	str = strings.TrimPrefix(str, "(")
	str = strings.TrimSuffix(str, ")")
	elems := strings.Split(str, ",")

	// Construct the vector.
	vector := make(vector.T, len(elems))
	for i, elem := range elems {
		elem = strings.TrimSpace(elem)
		value, err := strconv.ParseFloat(elem, 32)
		require.NoError(s.T, err)
		vector[i] = float32(value)
	}

	return vector
}

// parseKeyAndVector parses a line that may contain a key and vector separated
// by a colon. If there's no colon, it treats the line as just a key and gets
// the vector from the store.
func (s *testState) parseKeyAndVector(line string) (vecstore.PrimaryKey, vector.T) {
	parts := strings.Split(line, ":")
	if len(parts) == 1 {
		// Get the value from the store.
		key := vecstore.PrimaryKey(line)
		return key, s.InMemStore.GetVector(key)
	}

	// Parse the value after the colon.
	require.Len(s.T, parts, 2)
	key := vecstore.PrimaryKey(parts[0])
	return key, s.parseVector(parts[1])
}

func beginTransaction(ctx context.Context, t *testing.T, store vecstore.Store) vecstore.Txn {
	txn, err := store.Begin(ctx)
	require.NoError(t, err)
	return txn
}

func commitTransaction(ctx context.Context, t *testing.T, store vecstore.Store, txn vecstore.Txn) {
	err := store.Commit(ctx, txn)
	require.NoError(t, err)
}

// findMAP returns mean average precision, which compares a set of predicted
// results with the true set of results. Both sets are expected to be of equal
// length. It returns the percentage overlap of the predicted set with the truth
// set.
func findMAP(prediction, truth []vecstore.PrimaryKey) float64 {
	if len(prediction) != len(truth) {
		panic(errors.AssertionFailedf("prediction and truth sets are not same length"))
	}

	predictionMap := make(map[string]bool, len(prediction))
	for _, p := range prediction {
		predictionMap[string(p)] = true
	}

	var intersect float64
	for _, t := range truth {
		_, ok := predictionMap[string(t)]
		if ok {
			intersect++
		}
	}
	return intersect / float64(len(truth))
}

func TestRandomizeVector(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Create index.
	ctx := internal.WithWorkspace(context.Background(), &internal.Workspace{})
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)

	const dims = 97
	const count = 5
	quantizer := quantize.NewRaBitQuantizer(dims, 46)
	inMemStore := vecstore.NewInMemoryStore(dims, 42)
	index, err := NewVectorIndex(ctx, inMemStore, quantizer, 42, &VectorIndexOptions{}, stopper)
	require.NoError(t, err)

	// Generate random vectors with exponentially increasing norms, in order
	// make distances more distinct.
	rng := rand.New(rand.NewSource(42))
	data := make([]float32, dims*count)
	for i := range data {
		vecIdx := float64(i / dims)
		data[i] = float32(rng.NormFloat64() * math.Pow(1.5, vecIdx))
	}

	original := vector.MakeSetFromRawData(data, dims)
	randomized := vector.MakeSet(dims)
	randomized.AddUndefined(count)
	for i := range original.Count {
		index.randomizeVector(original.At(i), randomized.At(i))

		// Ensure that calling unRandomizeVector recovers original vector.
		randomizedInv := make([]float32, dims)
		index.unRandomizeVector(randomized.At(i), randomizedInv)
		for j, val := range original.At(i) {
			require.InDelta(t, val, randomizedInv[j], 0.00001)
		}
	}

	// Ensure that distances are similar, whether using the original vectors or
	// the randomized vectors.
	originalSet := quantizer.Quantize(ctx, original).(*quantize.RaBitQuantizedVectorSet)
	randomizedSet := quantizer.Quantize(ctx, randomized).(*quantize.RaBitQuantizedVectorSet)

	distances := make([]float32, count)
	errorBounds := make([]float32, count)
	quantizer.EstimateSquaredDistances(ctx, originalSet, original.At(0), distances, errorBounds)
	require.Equal(t, []float32{0, 272.75, 550.86, 950.93, 2421.41}, testutils.RoundFloats(distances, 2))
	require.Equal(t, []float32{37.58, 46.08, 57.55, 69.46, 110.57}, testutils.RoundFloats(errorBounds, 2))

	quantizer.EstimateSquaredDistances(ctx, randomizedSet, randomized.At(0), distances, errorBounds)
	require.Equal(t, []float32{5.1, 292.72, 454.95, 1011.85, 2475.87}, testutils.RoundFloats(distances, 2))
	require.Equal(t, []float32{37.58, 46.08, 57.55, 69.46, 110.57}, testutils.RoundFloats(errorBounds, 2))
}

// TestVectorIndexConcurrency builds an index on multiple goroutines, with
// background splits and merges enabled.
func TestVectorIndexConcurrency(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Create index.
	ctx := context.Background()
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)

	// Load features.
	vectors := testutils.LoadFeatures(t, 100)

	primaryKeys := make([]vecstore.PrimaryKey, vectors.Count)
	for i := 0; i < vectors.Count; i++ {
		primaryKeys[i] = vecstore.PrimaryKey(fmt.Sprintf("vec%d", i))
	}

	for i := 0; i < 10; i++ {
		options := VectorIndexOptions{
			MinPartitionSize: 2,
			MaxPartitionSize: 8,
			BaseBeamSize:     2,
			QualitySamples:   4,
		}
		seed := int64(i)
		store := vecstore.NewInMemoryStore(vectors.Dims, seed)
		quantizer := quantize.NewRaBitQuantizer(vectors.Dims, seed)
		index, err := NewVectorIndex(ctx, store, quantizer, seed, &options, stopper)
		require.NoError(t, err)

		buildIndex(ctx, t, store, index, vectors, primaryKeys)

		vectorCount := validateIndex(ctx, t, store)
		require.Equal(t, vectors.Count, vectorCount)

		index.Close()
	}
}

func buildIndex(
	ctx context.Context,
	t *testing.T,
	store *vecstore.InMemoryStore,
	index *VectorIndex,
	vectors vector.Set,
	primaryKeys []vecstore.PrimaryKey,
) {
	// Insert block of vectors within the scope of a transaction.
	insertBlock := func(start, end int) {
		for i := start; i < end; i++ {
			txn := beginTransaction(ctx, t, store)
			store.InsertVector(primaryKeys[i], vectors.At(i))
			require.NoError(t, index.Insert(ctx, txn, vectors.At(i), primaryKeys[i]))
			commitTransaction(ctx, t, store, txn)
		}
	}

	// Insert vectors into the store on multiple goroutines.
	var wait sync.WaitGroup
	procs := runtime.GOMAXPROCS(-1)
	countPerProc := (vectors.Count + procs) / procs
	blockSize := index.Options().MinPartitionSize
	for i := 0; i < vectors.Count; i += countPerProc {
		end := min(i+countPerProc, vectors.Count)
		wait.Add(1)
		go func(start, end int) {
			// Break vector group into individual transactions that each insert a
			// block of vectors. Run any pending fixups after each block.
			for j := start; j < end; j += blockSize {
				insertBlock(j, min(j+blockSize, end))
			}

			wait.Done()
		}(i, end)
	}
	wait.Wait()

	// Process any remaining fixups.
	index.ProcessFixups()
}

func validateIndex(ctx context.Context, t *testing.T, store *vecstore.InMemoryStore) int {
	txn := beginTransaction(ctx, t, store)
	defer commitTransaction(ctx, t, store, txn)

	vectorCount := 0
	partitionKeys := []vecstore.PartitionKey{vecstore.RootKey}
	for {
		// Get all child keys for next level.
		var childKeys []vecstore.ChildKey
		for _, key := range partitionKeys {
			partition, err := txn.GetPartition(ctx, key)
			if err != nil {
				panic(err)
			}
			require.NoError(t, err)
			childKeys = append(childKeys, partition.ChildKeys()...)
		}

		if len(childKeys) == 0 {
			break
		}

		// Verify full vectors exist for the level.
		refs := make([]vecstore.VectorWithKey, len(childKeys))
		for i := range childKeys {
			refs[i].Key = childKeys[i]
		}
		err := txn.GetFullVectors(ctx, refs)
		require.NoError(t, err)
		for i := range refs {
			if refs[i].Vector == nil {
				panic("vector is nil")
			}
			require.NotNil(t, refs[i].Vector)
		}

		// If this is not the leaf level, then process the next level.
		if childKeys[0].PrimaryKey == nil {
			partitionKeys = make([]vecstore.PartitionKey, len(childKeys))
			for i := range childKeys {
				partitionKeys[i] = childKeys[i].PartitionKey
			}
		} else {
			// This is the leaf level, so count vectors and end.
			vectorCount += len(childKeys)
			break
		}
	}

	return vectorCount
}
