package m3u8

/*
 Helpers for comparing and merging EXT-X-DATERANGE tags across reloads.

 rfc8216bis Section 4.4.5.1.1 governs updated Date Ranges: a later playlist
 carrying the same ID may add attributes that were previously absent, but an
 attribute that was present must keep the same value. START-DATE, CLASS,
 END-DATE, DURATION and the SCTE-35 attributes are immutable in that sense.
 A client-defined "X-" attribute that is absent in one version of the range
 may appear in a later one.
*/

import (
	"fmt"
	"sort"
	"time"
)

// wrapTimelineError annotates a sentinel error with context while keeping it
// discoverable through errors.Is.
func wrapTimelineError(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}

// xAttrsAsMap turns the client-defined attributes into a map, keeping the last
// value if a playlist repeats one. Attribute values already include their
// quoting, so they can be compared verbatim.
func xAttrsAsMap(dr *DateRange) map[string]string {
	out := make(map[string]string, len(dr.XAttrs))
	for _, a := range dr.XAttrs {
		out[a.Key] = a.Val
	}
	return out
}

// dateRangeCompatible reports whether two observations of the same ID may
// coexist or replace one another under the update rules. An observation may
// add previously absent attributes, including "X-" attributes, but must not
// change an attribute the earlier observation already carried.
func dateRangeCompatible(earlier, later *DateRange) bool {
	if !earlier.StartDate.Equal(later.StartDate) {
		return false
	}
	if earlier.Class != later.Class {
		return false
	}
	if earlier.EndDate != nil && later.EndDate != nil && !earlier.EndDate.Equal(*later.EndDate) {
		return false
	}
	if earlier.Duration != nil && later.Duration != nil && *earlier.Duration != *later.Duration {
		return false
	}
	if earlier.PlannedDuration != nil && later.PlannedDuration != nil &&
		*earlier.PlannedDuration != *later.PlannedDuration {
		return false
	}
	if earlier.Cue != "" && later.Cue != "" && earlier.Cue != later.Cue {
		return false
	}
	if earlier.SCTE35Cmd != "" && later.SCTE35Cmd != "" && earlier.SCTE35Cmd != later.SCTE35Cmd {
		return false
	}
	if earlier.SCTE35Out != "" && later.SCTE35Out != "" && earlier.SCTE35Out != later.SCTE35Out {
		return false
	}
	if earlier.SCTE35In != "" && later.SCTE35In != "" && earlier.SCTE35In != later.SCTE35In {
		return false
	}
	if earlier.EndOnNext && !later.EndOnNext {
		return false
	}
	earlierAttrs := xAttrsAsMap(earlier)
	for _, a := range later.XAttrs {
		if v, ok := earlierAttrs[a.Key]; ok && v != a.Val {
			return false
		}
	}
	return true
}

// dateRangeSemanticEqual reports whether an updated observation changes
// anything an event consumer could observe. It is used to suppress EventUpdate
// when the same range is repeated verbatim across reloads.
func dateRangeSemanticEqual(a, b *DateRange) bool {
	if !dateRangeCompatible(a, b) {
		return false
	}
	// dateRangeCompatible allows additions; equality requires the same set of
	// attributes with the same values.
	attrsA, attrsB := xAttrsAsMap(a), xAttrsAsMap(b)
	if len(attrsA) != len(attrsB) {
		return false
	}
	for k, v := range attrsA {
		if attrsB[k] != v {
			return false
		}
	}
	return a.Cue == b.Cue &&
		a.SCTE35Cmd == b.SCTE35Cmd &&
		a.SCTE35Out == b.SCTE35Out &&
		a.SCTE35In == b.SCTE35In &&
		optionalFloatEqual(a.Duration, b.Duration) &&
		optionalFloatEqual(a.PlannedDuration, b.PlannedDuration) &&
		optionalTimeEqual(a.EndDate, b.EndDate) &&
		a.EndOnNext == b.EndOnNext
}

// mergeDateRange combines two compatible observations, letting the later one
// fill in attributes the earlier one lacked.
func mergeDateRange(earlier, later *DateRange) *DateRange {
	merged := *earlier
	if merged.EndDate == nil && later.EndDate != nil {
		merged.EndDate = later.EndDate
	}
	if merged.Duration == nil && later.Duration != nil {
		merged.Duration = later.Duration
	}
	if merged.PlannedDuration == nil && later.PlannedDuration != nil {
		merged.PlannedDuration = later.PlannedDuration
	}
	if merged.Cue == "" {
		merged.Cue = later.Cue
	}
	if merged.SCTE35Cmd == "" {
		merged.SCTE35Cmd = later.SCTE35Cmd
	}
	if merged.SCTE35Out == "" {
		merged.SCTE35Out = later.SCTE35Out
	}
	if merged.SCTE35In == "" {
		merged.SCTE35In = later.SCTE35In
	}
	if later.EndOnNext {
		merged.EndOnNext = true
	}
	seen := xAttrsAsMap(&merged)
	for _, a := range later.XAttrs {
		if _, ok := seen[a.Key]; !ok {
			merged.XAttrs = append(merged.XAttrs, a)
			seen[a.Key] = a.Val
		}
	}
	sort.SliceStable(merged.XAttrs, func(i, j int) bool {
		return merged.XAttrs[i].Key < merged.XAttrs[j].Key
	})
	return &merged
}

func optionalFloatEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func optionalTimeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
