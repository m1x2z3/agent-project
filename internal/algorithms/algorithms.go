// Package algorithms contains small, independently testable algorithm exercises.
package algorithms

import "sort"

// Interval represents a closed integer interval from Start to End.
//
// MergeIntervals expects Start to be less than or equal to End. Invalid
// intervals are ignored because this exercise API does not return an error.
type Interval struct {
	Start int
	End   int
}

// BinarySearch returns the first index of target in an ascending sorted slice.
//
// It returns -1 when target is not present and does not modify sorted.
func BinarySearch(sorted []int, target int) int {
	left, right := 0, len(sorted)
	for left < right {
		middle := left + (right-left)/2
		if sorted[middle] < target {
			left = middle + 1
			continue
		}
		right = middle
	}

	if left < len(sorted) && sorted[left] == target {
		return left
	}
	return -1
}

// MergeIntervals sorts a copy of intervals and merges overlapping or touching
// intervals. The input slice is never modified.
func MergeIntervals(intervals []Interval) []Interval {
	if len(intervals) == 0 {
		return []Interval{}
	}

	sortedIntervals := append([]Interval(nil), intervals...)
	sort.Slice(sortedIntervals, func(i, j int) bool {
		if sortedIntervals[i].Start == sortedIntervals[j].Start {
			return sortedIntervals[i].End < sortedIntervals[j].End
		}
		return sortedIntervals[i].Start < sortedIntervals[j].Start
	})

	merged := make([]Interval, 0, len(sortedIntervals))
	for _, current := range sortedIntervals {
		if current.Start > current.End {
			continue
		}
		if len(merged) == 0 || current.Start > merged[len(merged)-1].End {
			merged = append(merged, current)
			continue
		}
		if current.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = current.End
		}
	}
	return merged
}
