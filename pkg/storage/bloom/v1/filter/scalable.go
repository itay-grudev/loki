/*
Original work Copyright (c) 2013 zhenjl
Modified work Copyright (c) 2015 Tyler Treat
Modified work Copyright (c) 2023 Owen Diehl
SPDX-License-Identifier: AGPL-3.0-only
Provenance-includes-location: https://github.com/tylertreat/BoomFilters/blob/master/scalable.go
Provenance-includes-location: https://github.com/owen-d/BoomFilters/blob/master/boom/scalable.go
Provenance-includes-license: Apache-2.0
Provenance-includes-license: MIT
Provenance-includes-copyright: The Loki Authors.


Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.
*/

package filter

import (
	"bytes"
	"encoding/binary"
	"hash"
	"io"
	"math"
)

// ScalableBloomFilter implements a Scalable Bloom Filter as described by
// Almeida, Baquero, Preguica, and Hutchison in Scalable Bloom Filters:
//
// http://gsd.di.uminho.pt/members/cbm/ps/dbloom.pdf
//
// A Scalable Bloom Filter dynamically adapts to the number of elements in the
// data set while enforcing a tight upper bound on the false-positive rate.
// This works by adding Bloom filters with geometrically decreasing
// false-positive rates as filters become full. The tightening ratio, r,
// controls the filter growth. The compounded probability over the whole series
// converges to a target value, even accounting for an infinite series.
//
// Scalable Bloom Filters are useful for cases where the size of the data set
// isn't known a priori and memory constraints aren't of particular concern.
// For situations where memory is bounded, consider using Inverse or Stable
// Bloom Filters.
type ScalableBloomFilter struct {
	filters []*PartitionedBloomFilter // filters with geometrically decreasing error rates
	r       float64                   // tightening ratio
	fp      float64                   // target false-positive rate
	p       float64                   // partition fill ratio
	hint    uint                      // filter size hint for first filter
	s       uint                      // space growth factor for successive filters. 2|4 recommended.

	// number of additions since last fill ratio check,
	// used to determine when to add a new filter.
	// Since fill ratios are estimated based on number of additions
	// and not actual fill ratio, this is used to amortize the cost
	// of checking the fill ratio.
	// Notably this is important when adding many duplicate keys to a filter
	// which does not increase the number of set bits, but can artificially inflate the estimated fill ratio
	// which tracks inserts.
	// Reset on adding another filter
	additionsSinceFillRatioCheck uint
}

const fillCheckFraction = 100

// NewScalableBloomFilter creates a new Scalable Bloom Filter with the
// specified target false-positive rate and tightening ratio. Use
// NewDefaultScalableBloomFilter if you don't want to calculate these
// parameters.
func NewScalableBloomFilter(hint uint, fpRate, r float64) *ScalableBloomFilter {
	s := &ScalableBloomFilter{
		filters: make([]*PartitionedBloomFilter, 0, 1),
		r:       r,
		fp:      fpRate,
		p:       fillRatio,
		hint:    hint,
		s:       4,
	}

	s.addFilter()
	return s
}

// NewDefaultScalableBloomFilter creates a new Scalable Bloom Filter with the
// specified target false-positive rate and an optimal tightening ratio.
func NewDefaultScalableBloomFilter(fpRate float64) *ScalableBloomFilter {
	return NewScalableBloomFilter(10000, fpRate, 0.8)
}

// Capacity returns the current Scalable Bloom Filter capacity, which is the
// sum of the capacities for the contained series of Bloom filters.
func (s *ScalableBloomFilter) Capacity() uint {
	capacity := uint(0)
	for _, bf := range s.filters {
		capacity += bf.Capacity()
	}
	return capacity
}

// K returns the number of hash functions used in each Bloom filter.
// Returns the highest value (the last filter)
func (s *ScalableBloomFilter) K() uint {
	return s.filters[len(s.filters)-1].K()
}

// FillRatio returns the average ratio of set bits across every filter.
func (s *ScalableBloomFilter) FillRatio() float64 {
	var sum, count float64
	for _, filter := range s.filters {
		capacity := filter.Capacity()
		sum += filter.FillRatio() * float64(capacity)
		count += float64(capacity)
	}
	return sum / count
}

// Test will test for membership of the data and returns true if it is a
// member, false if not. This is a probabilistic test, meaning there is a
// non-zero probability of false positives but a zero probability of false
// negatives.
func (s *ScalableBloomFilter) Test(data []byte) bool {
	// Querying is made by testing for the presence in each filter.
	for _, bf := range s.filters {
		if bf.Test(data) {
			return true
		}
	}

	return false
}

// Add will add the data to the Bloom filter. It returns the filter to allow
// for chaining.
func (s *ScalableBloomFilter) Add(data []byte) Filter {
	idx := len(s.filters) - 1

	// If the last filter has reached its fill ratio, add a new one.
	// While the estimated fill ratio is cheap to calculate, it overestimates how full a filter
	// may be because it doesn't account for duplicate key inserts.
	// Therefore, use the estimated fill ratio to determine when to add a new filter, but
	// throttle this by only checking the actual fill ratio when we've
	// performed inserts greater than some fraction of the filter's optimal cardinality
	// capacity since the last check.
	// This prevents us from running expensive fill ratio checks too often on both ends:
	// 1. When the filter is under utilized and the estimated fill ratio
	//    is below our target fill ratio
	// 2. When the filter is close to it's target utilization, duplicates inserts
	//    will quickly inflate the estimated fill ratio. By throttling this check to
	//    every n inserts where n is some fraction of the total optimal key count,
	//    we can amortize the cost of the fill ratio check.
	if s.filters[idx].EstimatedFillRatio() >= s.p && s.additionsSinceFillRatioCheck >= s.filters[idx].OptimalCount()/fillCheckFraction {
		s.additionsSinceFillRatioCheck = 0

		// calculate the actual fill ratio & update the estimated count for the filter. If the actual fill ratio
		// is above the target fill ratio, add a new filter.
		if ratio := s.filters[idx].UpdateCount(); ratio >= s.p {
			s.addFilter()
			idx++
		}

	}

	s.filters[idx].Add(data)
	s.additionsSinceFillRatioCheck++
	return s
}

// TestAndAdd is equivalent to calling Test followed by Add. It returns true if
// the data is a member, false if not.
func (s *ScalableBloomFilter) TestAndAdd(data []byte) bool {
	member := s.Test(data)
	s.Add(data)
	return member
}

// Reset restores the Bloom filter to its original state. It returns the filter
// to allow for chaining.
func (s *ScalableBloomFilter) Reset() *ScalableBloomFilter {
	s.filters = make([]*PartitionedBloomFilter, 0, 1)
	s.addFilter()
	return s
}

// addFilter adds a new Bloom filter with a restricted false-positive rate to
// the Scalable Bloom Filter
func (s *ScalableBloomFilter) addFilter() {
	fpRate := s.fp * math.Pow(s.r, float64(len(s.filters)))
	var p *PartitionedBloomFilter

	// first filter is created with a size determined by the hint.
	// successive filters are created with a size determined by the
	// previous filter's capacity and the space growth factor.
	if len(s.filters) == 0 {
		p = NewPartitionedBloomFilter(s.hint, fpRate)
	} else {
		p = NewPartitionedBloomFilterWithCapacity(s.filters[len(s.filters)-1].Capacity()*s.s, fpRate)
	}

	if len(s.filters) > 0 {
		p.SetHash(s.filters[0].hash)
	}
	s.filters = append(s.filters, p)
	s.additionsSinceFillRatioCheck = 0
}

// SetHash sets the hashing function used in the filter.
// For the effect on false positive rates see: https://github.com/tylertreat/BoomFilters/pull/1
func (s *ScalableBloomFilter) SetHash(h hash.Hash64) {
	for _, bf := range s.filters {
		bf.SetHash(h)
	}
}

// WriteTo writes a binary representation of the ScalableBloomFilter to an i/o stream.
// It returns the number of bytes written.
func (s *ScalableBloomFilter) WriteTo(stream io.Writer) (int64, error) {
	err := binary.Write(stream, binary.BigEndian, s.r)
	if err != nil {
		return 0, err
	}
	err = binary.Write(stream, binary.BigEndian, s.fp)
	if err != nil {
		return 0, err
	}
	err = binary.Write(stream, binary.BigEndian, s.p)
	if err != nil {
		return 0, err
	}
	err = binary.Write(stream, binary.BigEndian, uint64(s.hint))
	if err != nil {
		return 0, err
	}
	err = binary.Write(stream, binary.BigEndian, uint64(s.s))
	if err != nil {
		return 0, err
	}
	err = binary.Write(stream, binary.BigEndian, uint64(s.additionsSinceFillRatioCheck))
	if err != nil {
		return 0, err
	}
	err = binary.Write(stream, binary.BigEndian, uint64(len(s.filters)))
	if err != nil {
		return 0, err
	}
	var numBytes int64
	for _, filter := range s.filters {
		num, err := filter.WriteTo(stream)
		if err != nil {
			return 0, err
		}
		numBytes += num
	}
	return numBytes + int64(5*binary.Size(uint64(0))), err
}

// ReadFrom reads a binary representation of ScalableBloomFilter (such as might
// have been written by WriteTo()) from an i/o stream. It returns the number
// of bytes read.
func (s *ScalableBloomFilter) ReadFrom(stream io.Reader) (int64, error) {
	var r, fp, p float64
	var hint, growthFactor, additions, len uint64
	err := binary.Read(stream, binary.BigEndian, &r)
	if err != nil {
		return 0, err
	}
	err = binary.Read(stream, binary.BigEndian, &fp)
	if err != nil {
		return 0, err
	}
	err = binary.Read(stream, binary.BigEndian, &p)
	if err != nil {
		return 0, err
	}
	err = binary.Read(stream, binary.BigEndian, &hint)
	if err != nil {
		return 0, err
	}
	err = binary.Read(stream, binary.BigEndian, &growthFactor)
	if err != nil {
		return 0, err
	}
	err = binary.Read(stream, binary.BigEndian, &additions)
	if err != nil {
		return 0, err
	}
	err = binary.Read(stream, binary.BigEndian, &len)
	if err != nil {
		return 0, err
	}
	var numBytes int64
	filters := make([]*PartitionedBloomFilter, len)
	for i := range filters {
		filter := NewPartitionedBloomFilter(0, fp)
		num, err := filter.ReadFrom(stream)
		if err != nil {
			return 0, err
		}
		numBytes += num
		filters[i] = filter
	}
	s.r = r
	s.fp = fp
	s.p = p
	s.hint = uint(hint)
	s.s = uint(growthFactor)
	s.additionsSinceFillRatioCheck = uint(additions)
	s.filters = filters
	return numBytes + int64(5*binary.Size(uint64(0))), nil
}

// GobEncode implements gob.GobEncoder interface.
func (s *ScalableBloomFilter) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	_, err := s.WriteTo(&buf)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// GobDecode implements gob.GobDecoder interface.
func (s *ScalableBloomFilter) GobDecode(data []byte) error {
	buf := bytes.NewBuffer(data)
	_, err := s.ReadFrom(buf)

	return err
}

type ScalableBloomFilterLazyReader struct {
	filters []PartitionedBloomFilterLazyReader
}

func NewScalableBloomFilterLazyReader(data []byte) (ScalableBloomFilterLazyReader, int) {
	// Skip r, fp, p float64 and hint, s, additionsSinceFillRatioCheck uint64
	filtersLenOffset := 3*binary.Size(float64(0)) + 3*binary.Size(uint64(0))
	filtersLen := binary.BigEndian.Uint64(data[filtersLenOffset:])

	filterStartOffset := filtersLenOffset + binary.Size(uint64(0))

	filters := make([]PartitionedBloomFilterLazyReader, filtersLen)
	for i := range filters {
		filter, n := NewPartitionedBloomFilterLazyReader(data[filterStartOffset:])
		filterStartOffset += n
		filters[i] = filter
	}

	return ScalableBloomFilterLazyReader{
		filters: filters,
	}, filterStartOffset
}

// Test will test for membership of the data and returns true if it is a
// member, false if not. This is a probabilistic test, meaning there is a
// non-zero probability of false positives but a zero probability of false
// negatives.
func (s ScalableBloomFilterLazyReader) Test(data []byte) bool {
	// Querying is made by testing for the presence in each filter.
	for _, bf := range s.filters {
		if bf.Test(data) {
			return true
		}
	}

	return false
}
