// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package storepb

import (
	"strings"
	"unsafe"

	"github.com/gogo/protobuf/types"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/thanos-io/thanos/pkg/store/storepb/prompb"
)

var PartialResponseStrategyValues = func() []string {
	var s []string
	for k := range PartialResponseStrategy_value {
		s = append(s, k)
	}
	return s
}()

func NewWarnSeriesResponse(err error) *SeriesResponse {
	return &SeriesResponse{
		Result: &SeriesResponse_Warning{
			Warning: err.Error(),
		},
	}
}

func NewSeriesResponse(series *Series) *SeriesResponse {
	return &SeriesResponse{
		Result: &SeriesResponse_Series{
			Series: series,
		},
	}
}

func NewHintsSeriesResponse(hints *types.Any) *SeriesResponse {
	return &SeriesResponse{
		Result: &SeriesResponse_Hints{
			Hints: hints,
		},
	}
}

// CompareLabels compares two sets of labels.
// After lexicographical order, the set with fewer labels comes first.
func CompareLabels(a, b []Label) int {
	l := len(a)
	if len(b) < l {
		l = len(b)
	}
	for i := 0; i < l; i++ {
		if d := strings.Compare(a[i].Name, b[i].Name); d != 0 {
			return d
		}
		if d := strings.Compare(a[i].Value, b[i].Value); d != 0 {
			return d
		}
	}

	return len(a) - len(b)
}

type emptySeriesSet struct{}

func (emptySeriesSet) Next() bool                 { return false }
func (emptySeriesSet) At() ([]Label, []AggrChunk) { return nil, nil }
func (emptySeriesSet) Err() error                 { return nil }

// EmptySeriesSet returns a new series set that contains no series.
func EmptySeriesSet() SeriesSet {
	return emptySeriesSet{}
}

// MergeSeriesSets takes all series sets and returns as a union single series set.
// It assumes series are sorted by labels within single SeriesSet, similar to remote read guarantees.
// However, they can be partial: in such case, if the single SeriesSet returns the same series within many iterations,
// MergeSeriesSets will merge those into one.
//
// It also assumes in a "best effort" way that chunks are sorted by min time. It's done as an optimization only, so if input
// series' chunks are NOT sorted, the only consequence is that the duplicates might be not correctly removed. This is double checked
// which on just-before PromQL level as well, so the only consequence is increased network bandwidth.
// If all chunks were sorted, MergeSeriesSet ALSO returns sorted chunks by min time.
//
// Chunks within the same series can also overlap (within all SeriesSet
// as well as single SeriesSet alone). If the chunk ranges overlap, the *exact* chunk duplicates will be removed
// (except one), and any other overlaps will be appended into on chunks slice.
func MergeSeriesSets(all ...SeriesSet) SeriesSet {
	switch len(all) {
	case 0:
		return emptySeriesSet{}
	case 1:
		return newUniqueSeriesSet(all[0])
	}
	h := len(all) / 2

	return newMergedSeriesSet(
		MergeSeriesSets(all[:h]...),
		MergeSeriesSets(all[h:]...),
	)
}

// SeriesSet is a set of series and their corresponding chunks.
// The set is sorted by the label sets. Chunks may be overlapping or expected of order.
type SeriesSet interface {
	Next() bool
	At() ([]Label, []AggrChunk)
	Err() error
}

// mergedSeriesSet takes two series sets as a single series set.
type mergedSeriesSet struct {
	a, b SeriesSet

	lset         []Label
	chunks       []AggrChunk
	adone, bdone bool
}

func newMergedSeriesSet(a, b SeriesSet) *mergedSeriesSet {
	s := &mergedSeriesSet{a: a, b: b}
	// Initialize first elements of both sets as Next() needs
	// one element look-ahead.
	s.adone = !s.a.Next()
	s.bdone = !s.b.Next()

	return s
}

func (s *mergedSeriesSet) At() ([]Label, []AggrChunk) {
	return s.lset, s.chunks
}

func (s *mergedSeriesSet) Err() error {
	if s.a.Err() != nil {
		return s.a.Err()
	}
	return s.b.Err()
}

func (s *mergedSeriesSet) compare() int {
	if s.adone {
		return 1
	}
	if s.bdone {
		return -1
	}
	lsetA, _ := s.a.At()
	lsetB, _ := s.b.At()
	return CompareLabels(lsetA, lsetB)
}

func (s *mergedSeriesSet) Next() bool {
	if s.adone && s.bdone || s.Err() != nil {
		return false
	}

	d := s.compare()
	if d > 0 {
		s.lset, s.chunks = s.b.At()
		s.bdone = !s.b.Next()
		return true
	}
	if d < 0 {
		s.lset, s.chunks = s.a.At()
		s.adone = !s.a.Next()
		return true
	}

	// Both a and b contains the same series. Go through all chunks, remove duplicates and concatenate chunks from both
	// series sets. We best effortly assume chunks are sorted by min time. If not, we will not detect all deduplicate which will
	// be account on select layer anyway. We do it still for early optimization.
	lset, chksA := s.a.At()
	_, chksB := s.b.At()
	s.lset = lset

	// Slice reuse is not generally safe with nested merge iterators.
	// We err on the safe side an create a new slice.
	s.chunks = make([]AggrChunk, 0, len(chksA)+len(chksB))

	b := 0
Outer:
	for a := range chksA {
		for {
			if b >= len(chksB) {
				// No more b chunks.
				s.chunks = append(s.chunks, chksA[a:]...)
				break Outer
			}

			if chksA[a].MinTime < chksB[b].MinTime {
				s.chunks = append(s.chunks, chksA[a])
				break
			}

			if chksA[a].MinTime > chksB[b].MinTime {
				s.chunks = append(s.chunks, chksB[b])
				b++
				continue
			}

			// TODO(bwplotka): This is expensive.
			//fmt.Println("check strings")
			if strings.Compare(chksA[a].String(), chksB[b].String()) == 0 {
				// Exact duplicated chunks, discard one from b.
				b++
				continue
			}

			// Same min Time, but not duplicate, so it does not matter. Take b (since lower for loop).
			s.chunks = append(s.chunks, chksB[b])
			b++
		}
	}

	if b < len(chksB) {
		s.chunks = append(s.chunks, chksB[b:]...)
	}

	s.adone = !s.a.Next()
	s.bdone = !s.b.Next()
	return true
}

// uniqueSeriesSet takes one series set and ensures each iteration contains single, full series.
type uniqueSeriesSet struct {
	SeriesSet
	done bool

	peek *Series

	lset   []Label
	chunks []AggrChunk
}

func newUniqueSeriesSet(wrapped SeriesSet) *uniqueSeriesSet {
	return &uniqueSeriesSet{SeriesSet: wrapped}
}

func (s *uniqueSeriesSet) At() ([]Label, []AggrChunk) {
	return s.lset, s.chunks
}

func (s *uniqueSeriesSet) Next() bool {
	if s.Err() != nil {
		return false
	}

	for !s.done {
		if s.done = !s.SeriesSet.Next(); s.done {
			break
		}
		lset, chks := s.SeriesSet.At()
		if s.peek == nil {
			s.peek = &Series{Labels: lset, Chunks: chks}
			continue
		}

		if CompareLabels(lset, s.peek.Labels) != 0 {
			s.lset, s.chunks = s.peek.Labels, s.peek.Chunks
			s.peek = &Series{Labels: lset, Chunks: chks}
			return true
		}

		// We assume non-overlapping, sorted chunks. This is best effort only, if it's otherwise it
		// will just be duplicated, but well handled by StoreAPI consumers.
		s.peek.Chunks = append(s.peek.Chunks, chks...)

	}

	if s.peek == nil {
		return false
	}

	s.lset, s.chunks = s.peek.Labels, s.peek.Chunks
	s.peek = nil
	return true
}

// LabelsToPromLabels converts Thanos proto labels to Prometheus labels in type safe manner.
func LabelsToPromLabels(lset []Label) labels.Labels {
	ret := make(labels.Labels, len(lset))
	for i, l := range lset {
		ret[i] = labels.Label{Name: l.Name, Value: l.Value}
	}
	return ret
}

// LabelsToPromLabelsUnsafe converts Thanos proto labels to Prometheus labels in type unsafe manner.
// It reuses the same memory. Caller should abort using passed []Labels.
//
// NOTE: This depends on order of struct fields etc, so use with extreme care.
func LabelsToPromLabelsUnsafe(lset []Label) labels.Labels {
	return *(*[]labels.Label)(unsafe.Pointer(&lset))
}

// PromLabelsToLabels converts Prometheus labels to Thanos proto labels in type safe manner.
func PromLabelsToLabels(lset labels.Labels) []Label {
	ret := make([]Label, len(lset))
	for i, l := range lset {
		ret[i] = Label{Name: l.Name, Value: l.Value}
	}
	return ret
}

// PromLabelsToLabelsUnsafe converts Prometheus labels to Thanos proto labels in type unsafe manner.
// It reuses the same memory. Caller should abort using passed labels.Labels.
//
// // NOTE: This depends on order of struct fields etc, so use with extreme care.
func PromLabelsToLabelsUnsafe(lset labels.Labels) []Label {
	return *(*[]Label)(unsafe.Pointer(&lset))
}

// PrompbLabelsToLabels converts Prometheus labels to Thanos proto labels in type safe manner.
func PrompbLabelsToLabels(lset []prompb.Label) []Label {
	ret := make([]Label, len(lset))
	for i, l := range lset {
		ret[i] = Label{Name: l.Name, Value: l.Value}
	}
	return ret
}

// PrompbLabelsToLabelsUnsafe converts Prometheus proto labels to Thanos proto labels in type unsafe manner.
// It reuses the same memory. Caller should abort using passed labels.Labels.
//
// // NOTE: This depends on order of struct fields etc, so use with extreme care.
func PrompbLabelsToLabelsUnsafe(lset []prompb.Label) []Label {
	return *(*[]Label)(unsafe.Pointer(&lset))
}

func LabelsToString(lset []Label) string {
	var s []string
	for _, l := range lset {
		s = append(s, l.String())
	}
	return "[" + strings.Join(s, ",") + "]"
}

func LabelSetsToString(lsets []LabelSet) string {
	s := []string{}
	for _, ls := range lsets {
		s = append(s, LabelsToString(ls.Labels))
	}
	return strings.Join(s, "")
}
