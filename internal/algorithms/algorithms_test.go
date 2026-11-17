package algorithms

import (
	"reflect"
	"testing"
)

func TestBinarySearch(t *testing.T) {
	tests := []struct {
		name   string
		values []int
		target int
		want   int
	}{
		{name: "finds first duplicate", values: []int{1, 2, 2, 2, 5}, target: 2, want: 1},
		{name: "finds first element", values: []int{1, 2, 3}, target: 1, want: 0},
		{name: "finds last element", values: []int{1, 2, 3}, target: 3, want: 2},
		{name: "returns not found", values: []int{1, 2, 3}, target: 4, want: -1},
		{name: "handles empty input", values: []int{}, target: 1, want: -1},
		{name: "handles single element", values: []int{7}, target: 7, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := BinarySearch(test.values, test.target); got != test.want {
				t.Fatalf("BinarySearch(%v, %d) = %d, want %d", test.values, test.target, got, test.want)
			}
		})
	}
}

func TestMergeIntervals(t *testing.T) {
	tests := []struct {
		name      string
		intervals []Interval
		want      []Interval
	}{
		{
			name:      "handles empty input",
			intervals: []Interval{},
			want:      []Interval{},
		},
		{
			name:      "merges unordered overlapping intervals",
			intervals: []Interval{{Start: 5, End: 7}, {Start: 1, End: 3}, {Start: 2, End: 6}},
			want:      []Interval{{Start: 1, End: 7}},
		},
		{
			name:      "merges touching intervals",
			intervals: []Interval{{Start: 1, End: 3}, {Start: 3, End: 5}},
			want:      []Interval{{Start: 1, End: 5}},
		},
		{
			name:      "keeps disjoint intervals",
			intervals: []Interval{{Start: 1, End: 2}, {Start: 5, End: 8}},
			want:      []Interval{{Start: 1, End: 2}, {Start: 5, End: 8}},
		},
		{
			name:      "merges fully contained intervals",
			intervals: []Interval{{Start: 1, End: 10}, {Start: 3, End: 5}, {Start: 2, End: 8}},
			want:      []Interval{{Start: 1, End: 10}},
		},
		{
			name:      "ignores invalid intervals",
			intervals: []Interval{{Start: 5, End: 2}, {Start: 1, End: 3}},
			want:      []Interval{{Start: 1, End: 3}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := append([]Interval(nil), test.intervals...)
			got := MergeIntervals(input)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("MergeIntervals(%v) = %v, want %v", test.intervals, got, test.want)
			}
		})
	}
}

func TestMergeIntervalsDoesNotModifyInput(t *testing.T) {
	input := []Interval{{Start: 5, End: 7}, {Start: 1, End: 3}, {Start: 2, End: 6}}
	original := append([]Interval(nil), input...)

	_ = MergeIntervals(input)

	if !reflect.DeepEqual(input, original) {
		t.Fatalf("input was modified: got %v, want %v", input, original)
	}
}
