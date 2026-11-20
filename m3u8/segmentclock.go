package m3u8

/*
 This file maps program time onto the segments of a media playlist.

 EXT-X-DATERANGE tags are stamped in absolute program time, while
 EXT-X-DISCONTINUITY tags live on segments. To say which discontinuity a range
 event belongs to, the segment clock walks the playlist segments from their
 EXT-X-PROGRAM-DATE-TIME anchors and records the program-time boundary at which
 every EXT-X-DISCONTINUITY happens.

 rfc8216bis Section 4.4.2.6 says EXT-X-PROGRAM-DATE-TIME applies to the first
 sample of the media segment that follows it, and Section 4.4.4.3 says a
 discontinuity resets the timestamp continuity, so each anchor is trusted.
*/

import "time"

// segmentClock holds the program-time intervals covered by the segments of one
// playlist reload.
type segmentClock struct {
	// hasProgramTime is false when no segment carries an
	// EXT-X-PROGRAM-DATE-TIME, in which case program time cannot be mapped.
	hasProgramTime bool
	// start is the program time at which each segment starts, in playlist
	// order. A later anchor overrides a duration-extrapolated one.
	start []time.Time
	// end is the program time at which each segment ends.
	end []time.Time
	// boundaries are the program times of the first sample after each
	// EXT-X-DISCONTINUITY, paired with the discontinuity sequence in force
	// from that time.
	boundaries []discontinuityBoundary
	// baseDiscontinuity is the EXT-X-DISCONTINUITY-SEQUENCE of the first
	// segment; the specification defaults it to zero.
	baseDiscontinuity uint64
}

type discontinuityBoundary struct {
	at  time.Time
	seq uint64
}

// buildSegmentClock derives the segment clock of a media playlist.
func buildSegmentClock(p *MediaPlaylist) segmentClock {
	clock := segmentClock{
		baseDiscontinuity: p.DiscontinuitySeq,
	}
	n := len(p.Segments)
	if n == 0 {
		clock.hasProgramTime = false
		return clock
	}
	clock.start = make([]time.Time, n)
	clock.end = make([]time.Time, n)

	// The discontinuity sequence of segment i is the playlist's
	// EXT-X-DISCONTINUITY-SEQUENCE plus the number of discontinuity tags at
	// or before it.
	discoSeq := p.DiscontinuitySeq
	var current time.Time
	haveTime := false
	for i, seg := range p.Segments {
		if seg == nil {
			continue
		}
		if seg.Discontinuity {
			// The boundary is the first sample of this segment, whose time is
			// resolved below; record it once that time is known.
			discoSeq++
		}
		if !seg.ProgramDateTime.IsZero() {
			// A fresh anchor is authoritative, including after a discontinuity:
			// rfc8216bis Section 4.4.2.6 associates it with the first sample
			// of the following segment.
			current = seg.ProgramDateTime
			haveTime = true
			clock.hasProgramTime = true
		} else if haveTime {
			current = current.Add(segmentDuration(seg))
		}
		clock.start[i] = current
		clock.end[i] = current.Add(segmentDuration(seg))
		if haveTime && seg.Discontinuity {
			clock.boundaries = append(clock.boundaries,
				discontinuityBoundary{at: current, seq: discoSeq})
		}
		if haveTime {
			current = clock.end[i]
		}
	}
	return clock
}

// windowStart returns the program time of the first sample of the oldest
// segment in the playlist, which is the lower edge of the sliding window.
func (c segmentClock) windowStart() (time.Time, bool) {
	for _, at := range c.start {
		if !at.IsZero() {
			return at, true
		}
	}
	return time.Time{}, false
}

// discontinuityAt returns the discontinuity sequence in force at a program
// time. Times before the first known boundary map onto the playlist's base
// sequence, which also covers ranges starting before the first segment.
func (c segmentClock) discontinuityAt(at time.Time) uint64 {
	seq := c.baseDiscontinuity
	for _, b := range c.boundaries {
		if at.Before(b.at) {
			break
		}
		seq = b.seq
	}
	return seq
}

// segmentDuration is the segment's EXTINF duration as a time.Duration.
func segmentDuration(seg *MediaSegment) time.Duration {
	return time.Duration(seg.Duration * float64(time.Second))
}

// durationDuration converts an EXT-X-DATERANGE DURATION, which is expressed in
// seconds, to a time.Duration.
func durationDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}
