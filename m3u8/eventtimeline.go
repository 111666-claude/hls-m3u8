package m3u8

/*
 This file implements an event timeline built from EXT-X-DATERANGE tags.

 A live media playlist is reloaded again and again, and its sliding window
 drops old segments and the tags that went with them. An EXT-X-DATERANGE that
 advertises an ad or a chapter can therefore appear in several consecutive
 reloads, so an application cannot raise an event for every tag it sees. The
 EventTimeline keeps the state of the date ranges it has already observed and
 turns each reloaded MediaPlaylist into Start, Update and End events on the
 program clock, so every range starts once even when it is seen many times.

 The rules followed here are the EXT-X-DATERANGE rules of rfc8216bis
 Section 4.4.5.1, in particular the rules for carrying an attribute in an
 updated Date Range (4.4.5.1.1) and the END-ON-NEXT attribute (4.4.5.1.2).
*/

import (
	"errors"
	"sort"
	"time"
)

// Sentinel errors reported by EventTimeline.Update. They are wrapped, so use
// errors.Is rather than comparing the returned error directly.
var (
	// ErrMissingProgramDate reports that a date range or a media playlist
	// carries no absolute program time. A date range without START-DATE has
	// no position on the program clock, and a playlist whose segments carry
	// no EXT-X-PROGRAM-DATE-TIME gives no clock to map ranges onto.
	ErrMissingProgramDate = errors.New("missing program date")

	// ErrInvalidDateRangeInterval reports that an END-DATE or a DURATION ends
	// before START-DATE, which rfc8216bis Section 4.4.5.1 forbids.
	ErrInvalidDateRangeInterval = errors.New("invalid date range interval")

	// ErrDuplicateDateRangeID reports that one reload contains two date
	// ranges with the same ID but incompatible immutable attributes. The
	// specification says such a playlist MUST be rejected.
	ErrDuplicateDateRangeID = errors.New("duplicate date range ID conflict")

	// ErrDateRangeOutOfOrder reports that date ranges or the playlist window
	// move backwards on the program clock, for example overlapping reloads
	// whose EXT-X-MEDIA-SEQUENCE goes backwards.
	ErrDateRangeOutOfOrder = errors.New("date ranges out of order")
)

// defaultMaxTrackedRanges bounds the state kept for ranges that have left the
// sliding window. Program-time pruning normally keeps the state smaller; the
// bound is the backstop that makes a long-lived live timeline finite when no
// program clock is available.
const defaultMaxTrackedRanges = 1000

// EventType is the kind of a TimelineEvent.
type EventType uint

const (
	// EventStart is emitted the first time a date range ID is seen.
	EventStart EventType = iota + 1
	// EventUpdate is emitted when a known range reappears with changed
	// attributes, for example when an open ad gains an END-DATE.
	EventUpdate
	// EventEnd is emitted when a range becomes known to have ended.
	EventEnd
)

func (e EventType) String() string {
	switch e {
	case EventStart:
		return "start"
	case EventUpdate:
		return "update"
	case EventEnd:
		return "end"
	}
	return "unknown"
}

// TimelineEvent is a single change of the event timeline on the program clock.
type TimelineEvent struct {
	// Type is Start, Update or End.
	Type EventType
	// At is the program time the event refers to. For Start and Update it is
	// the range's START-DATE; for End it is the resolved end time.
	At time.Time
	// Range is the date range as known after the reload that produced the
	// event, so an Update event already carries the new attributes. A
	// returned slice and its ranges must not be modified.
	Range *DateRange
	// DiscontinuitySeq is the EXT-X-DISCONTINUITY-SEQUENCE of the media at
	// At. DiscontinuityKnown is false when the playlist gives no
	// EXT-X-PROGRAM-DATE-TIME to map the time onto a discontinuity.
	DiscontinuitySeq uint64
	// DiscontinuityKnown says whether DiscontinuitySeq was resolved from the
	// playlist's segments.
	DiscontinuityKnown bool
}

// EventTimeline turns reloaded media playlists into Start, Update and End
// events. Create one with NewEventTimeline and call Update once per reloaded
// MediaPlaylist, keeping the same instance across reloads.
//
// The zero value is not usable. An EventTimeline is not safe for concurrent
// use; guard it if several goroutines reload playlists into it.
type EventTimeline struct {
	strict          bool
	maxTracked      int
	ranges          map[string]*trackedRange
	lastClock       segmentClock
	lastSeqNo       uint64
	haveLastSeqNo   bool
	lastWindowStart time.Time
	haveWindowStart bool
	refreshCount    uint64
}

// EventTimelineOption configures an EventTimeline at creation.
type EventTimelineOption func(*EventTimeline)

// WithStrictTimeline makes Update return the first specification violation it
// finds instead of skipping the offending range and continuing. A non-strict
// timeline never returns one of the sentinel errors: ranges without a program
// date are ignored, invalid intervals and conflicting duplicate IDs are
// skipped, and backwards-moving reloads reset the timeline so tracking starts
// afresh.
func WithStrictTimeline() EventTimelineOption {
	return func(t *EventTimeline) { t.strict = true }
}

// WithMaxTrackedRanges bounds the number of ranges the timeline keeps state
// for after they have left the playlist window. When the bound is reached the
// oldest ended ranges are forgotten first, then, if necessary, the oldest
// ranges overall. A limit of zero removes the bound; program-time pruning
// still applies whenever the playlist carries an EXT-X-PROGRAM-DATE-TIME.
func WithMaxTrackedRanges(limit int) EventTimelineOption {
	return func(t *EventTimeline) { t.maxTracked = limit }
}

// NewEventTimeline creates an EventTimeline ready for repeated calls to Update.
func NewEventTimeline(options ...EventTimelineOption) *EventTimeline {
	t := &EventTimeline{
		maxTracked: defaultMaxTrackedRanges,
		ranges:     make(map[string]*trackedRange),
	}
	for _, option := range options {
		option(t)
	}
	return t
}

// trackedRange is the state kept for one EXT-X-DATERANGE ID across reloads.
type trackedRange struct {
	range_    *DateRange
	ended     bool
	endAt     time.Time
	firstSeen time.Time // START-DATE of the first observation, for ordering
}

// Update ingests one reloaded media playlist and returns the events produced
// by that reload, ordered on the program clock. A range ID emits EventStart
// the first time it is seen, EventUpdate only when its attributes change, and
// EventEnd when an END-DATE, DURATION or an END-ON-NEXT successor proves it
// has ended. Ranges that disappeared from the sliding window keep their state,
// so a range never starts twice.
//
// In strict mode the first violation is returned wrapped in one of the
// package's Err*DateRange sentinel errors and no state changes are applied for
// the reload. In non-strict mode invalid ranges are skipped.
func (t *EventTimeline) Update(p *MediaPlaylist) ([]TimelineEvent, error) {
	if p == nil {
		return nil, nil
	}
	t.refreshCount++

	segClock := buildSegmentClock(p)
	windowStart, haveWindow := segClock.windowStart()

	if err := t.checkWindowOrder(p, haveWindow, windowStart); err != nil {
		return nil, err
	}

	collected, err := t.collect(p, segClock)
	if err != nil {
		return nil, err
	}

	// In strict mode no state of the failed reload may survive, so every
	// mutation below happens on a snapshot that replaces the live state only
	// when processing completes. Non-strict mode never fails here.
	snapshot := t.snapshotState()
	ranges := snapshot

	events := make([]TimelineEvent, 0, len(collected))

	// Resolve END-ON-NEXT before any events, since a predecessor may end in
	// the same reload in which its successor starts.
	endOnNextEnds := t.resolveEndOnNext(collected)

	for i := range collected {
		dr := collected[i]

		existing, known := ranges[dr.ID]
		if !known {
			tr := &trackedRange{
				range_:    dr,
				firstSeen: dr.StartDate,
			}
			if end, ok := endOnNextEnds[dr.ID]; ok {
				tr.ended = true
				tr.endAt = end
			} else if end, ok := explicitEnd(dr); ok {
				tr.ended = true
				tr.endAt = end
			}
			ranges[dr.ID] = tr
			events = append(events, t.event(EventStart, dr.StartDate, tr, segClock))
			if tr.ended {
				events = append(events, t.event(EventEnd, tr.endAt, tr, segClock))
			}
			continue
		}

		// An updated observation may only add attributes, never change ones
		// already carried (rfc8216bis 4.4.5.1.1).
		if !dateRangeCompatible(existing.range_, dr) {
			if t.strict {
				return nil, wrapTimelineError(ErrDuplicateDateRangeID,
					"date range %q changed an immutable attribute in a later playlist", dr.ID)
			}
			continue
		}
		changed := !dateRangeSemanticEqual(existing.range_, dr)
		existing.range_ = mergeDateRange(existing.range_, dr)
		if changed {
			events = append(events, t.event(EventUpdate, dr.StartDate, existing, segClock))
		}
		if !existing.ended {
			if end, ok := endOnNextEnds[dr.ID]; ok {
				existing.ended = true
				existing.endAt = end
				events = append(events, t.event(EventEnd, end, existing, segClock))
			} else if end, ok := explicitEnd(dr); ok {
				existing.ended = true
				existing.endAt = end
				events = append(events, t.event(EventEnd, end, existing, segClock))
			}
		}
	}

	// A range no longer present is remembered, but its end time stops moving
	// with the window, which makes it a pruning candidate below.
	if haveWindow {
		pruneRanges(ranges, windowStart)
	}
	enforceBound(ranges, t.maxTracked)

	t.ranges = ranges
	t.lastClock = segClock
	t.lastSeqNo = p.SeqNo
	t.haveLastSeqNo = true
	if haveWindow {
		t.lastWindowStart = windowStart
		t.haveWindowStart = true
	}

	sortTimelineEvents(events)
	return events, nil
}

// snapshotState returns a shallow-copied map of the tracked ranges so a failed
// strict reload can leave the timeline untouched. The trackedRange values are
// copied as well; the DateRanges themselves are read-only playlist data.
func (t *EventTimeline) snapshotState() map[string]*trackedRange {
	out := make(map[string]*trackedRange, len(t.ranges))
	for id, tr := range t.ranges {
		copyTR := *tr
		out[id] = &copyTR
	}
	return out
}

// RefreshCount returns how many times Update has been called, including calls
// that produced no events. It helps tests and applications correlate events
// with playlist reloads.
func (t *EventTimeline) RefreshCount() uint64 {
	return t.refreshCount
}

// ActiveRanges returns the ranges currently tracked that have not ended, in
// START-DATE order. The returned ranges must not be modified.
func (t *EventTimeline) ActiveRanges() []*DateRange {
	out := make([]*DateRange, 0, len(t.ranges))
	for _, tr := range t.ranges {
		if !tr.ended {
			out = append(out, tr.range_)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartDate.Equal(out[j].StartDate) {
			return out[i].StartDate.Before(out[j].StartDate)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DiscontinuityAt maps a program time to the EXT-X-DISCONTINUITY-SEQUENCE of
// the media playing at that time. The boolean is false when the playlist used
// for the last Update carried no EXT-X-PROGRAM-DATE-TIME and the time cannot
// be mapped.
func (t *EventTimeline) DiscontinuityAt(at time.Time) (uint64, bool) {
	if !t.lastClock.hasProgramTime {
		return 0, false
	}
	return t.lastClock.discontinuityAt(at), true
}

// checkWindowOrder guards against reloads that move backwards, which would
// otherwise replay starts. Non-strict timelines reset and carry on.
func (t *EventTimeline) checkWindowOrder(p *MediaPlaylist, haveWindow bool, windowStart time.Time) error {
	if t.haveLastSeqNo && p.SeqNo < t.lastSeqNo {
		if t.strict {
			return wrapTimelineError(ErrDateRangeOutOfOrder,
				"EXT-X-MEDIA-SEQUENCE went from %d to %d", t.lastSeqNo, p.SeqNo)
		}
		t.reset()
		return nil
	}
	if t.haveWindowStart && haveWindow && windowStart.Before(t.lastWindowStart) {
		if t.strict {
			return wrapTimelineError(ErrDateRangeOutOfOrder,
				"window start went from %s to %s", t.lastWindowStart, windowStart)
		}
		t.reset()
	}
	return nil
}

// collect gathers all date ranges of a reload, wherever the decoder stored
// them, and applies the validation rules of rfc8216bis Section 4.4.5.1.
func (t *EventTimeline) collect(p *MediaPlaylist, clock segmentClock) ([]*DateRange, error) {
	capacity := len(p.DateRanges) + len(p.TrailingDateRanges)
	for _, seg := range p.Segments {
		if seg != nil {
			capacity += len(seg.SCTE35DateRanges)
		}
	}
	all := make([]*DateRange, 0, capacity)
	all = append(all, p.DateRanges...)
	all = append(all, p.TrailingDateRanges...)
	for _, seg := range p.Segments {
		if seg != nil {
			all = append(all, seg.SCTE35DateRanges...)
		}
	}

	// Duplicate IDs within one playlist: identical or complementary
	// observations merge, conflicting ones are invalid (rfc8216bis 4.4.5.1.1).
	byID := make(map[string]*DateRange, len(all))
	collected := make([]*DateRange, 0, len(all))
	var prevStart time.Time
	havePrev := false
	// outOfOrder remembers a backwards START-DATE so a non-strict timeline can
	// sort the ranges into program-time order instead of emitting them in the
	// order a malformed playlist happened to use.
	outOfOrder := false
	for _, dr := range all {
		if dr == nil {
			continue
		}
		if dr.StartDate.IsZero() {
			if t.strict {
				return nil, wrapTimelineError(ErrMissingProgramDate,
					"date range %q has no START-DATE", dr.ID)
			}
			continue
		}
		if end, ok := explicitEnd(dr); ok && end.Before(dr.StartDate) {
			if t.strict {
				return nil, wrapTimelineError(ErrInvalidDateRangeInterval,
					"date range %q ends (%s) before it starts (%s)",
					dr.ID, end, dr.StartDate)
			}
			continue
		}
		if havePrev && dr.StartDate.Before(prevStart) {
			if t.strict {
				return nil, wrapTimelineError(ErrDateRangeOutOfOrder,
					"date range %q starts at %s after a range starting at %s",
					dr.ID, dr.StartDate, prevStart)
			}
			outOfOrder = true
		}
		prevStart = dr.StartDate
		havePrev = true

		if existing, ok := byID[dr.ID]; ok {
			if !dateRangeCompatible(existing, dr) {
				if t.strict {
					return nil, wrapTimelineError(ErrDuplicateDateRangeID,
						"date range %q appears twice with conflicting attributes", dr.ID)
				}
				continue
			}
			merged := mergeDateRange(existing, dr)
			byID[dr.ID] = merged
			for i := range collected {
				if collected[i].ID == dr.ID {
					collected[i] = merged
					break
				}
			}
			continue
		}
		byID[dr.ID] = dr
		collected = append(collected, dr)
	}

	// A playlist whose segments have no EXT-X-PROGRAM-DATE-TIME leaves the
	// timeline without a clock for window pruning and discontinuity mapping.
	// The ranges still carry absolute START-DATEs, so a non-strict timeline
	// continues and simply disables those features.
	if !clock.hasProgramTime && len(collected) > 0 && t.strict {
		return nil, wrapTimelineError(ErrMissingProgramDate,
			"media playlist has no EXT-X-PROGRAM-DATE-TIME to map date ranges onto")
	}
	if outOfOrder {
		sort.SliceStable(collected, func(i, j int) bool {
			if !collected[i].StartDate.Equal(collected[j].StartDate) {
				return collected[i].StartDate.Before(collected[j].StartDate)
			}
			return collected[i].ID < collected[j].ID
		})
	}
	return collected, nil
}

// resolveEndOnNext applies END-ON-NEXT: a range with that attribute ends where
// the next range of the same CLASS begins (rfc8216bis 4.4.5.1.2). The returned
// map is keyed by the ending range's ID.
func (t *EventTimeline) resolveEndOnNext(collected []*DateRange) map[string]time.Time {
	ends := make(map[string]time.Time)
	byClass := make(map[string][]*DateRange)
	for _, dr := range collected {
		if dr.Class != "" {
			byClass[dr.Class] = append(byClass[dr.Class], dr)
		}
	}
	for _, ranges := range byClass {
		sort.Slice(ranges, func(i, j int) bool {
			if !ranges[i].StartDate.Equal(ranges[j].StartDate) {
				return ranges[i].StartDate.Before(ranges[j].StartDate)
			}
			return ranges[i].ID < ranges[j].ID
		})
		for i := 0; i < len(ranges)-1; i++ {
			cur, next := ranges[i], ranges[i+1]
			if cur.EndOnNext && !next.StartDate.Before(cur.StartDate) {
				ends[cur.ID] = next.StartDate
			}
		}
	}
	// A successor may already be tracked from an earlier reload.
	for id, tr := range t.ranges {
		if tr.ended || !tr.range_.EndOnNext {
			continue
		}
		if _, ok := ends[id]; ok {
			continue
		}
		for _, dr := range collected {
			if dr.Class == tr.range_.Class && !dr.StartDate.Before(tr.range_.StartDate) && dr.ID != id {
				ends[id] = dr.StartDate
				break
			}
		}
	}
	return ends
}

// pruneRanges forgets ranges whose whole interval lies before the current
// window. Open-ended ranges are always kept, because an END-DATE can arrive in
// a later reload after the start has already slid out of the window.
func pruneRanges(ranges map[string]*trackedRange, windowStart time.Time) {
	for id, tr := range ranges {
		if !tr.ended || tr.endAt.IsZero() {
			continue
		}
		if tr.endAt.Before(windowStart) {
			delete(ranges, id)
		}
	}
}

// enforceBound keeps the tracked state finite when no program clock allows
// program-time pruning. Ended ranges are forgotten first.
func enforceBound(ranges map[string]*trackedRange, maxTracked int) {
	if maxTracked <= 0 || len(ranges) <= maxTracked {
		return
	}
	all := make([]*trackedRange, 0, len(ranges))
	for _, tr := range ranges {
		all = append(all, tr)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ended != all[j].ended {
			return all[i].ended // ended ranges first in pruning order
		}
		if !all[i].firstSeen.Equal(all[j].firstSeen) {
			return all[i].firstSeen.Before(all[j].firstSeen)
		}
		return all[i].range_.ID < all[j].range_.ID
	})
	excess := len(ranges) - maxTracked
	for _, tr := range all {
		if excess == 0 {
			break
		}
		delete(ranges, tr.range_.ID)
		excess--
	}
}

// reset drops all tracked state, used when a non-strict timeline sees a
// backwards-moving reload so no range replays its start.
func (t *EventTimeline) reset() {
	t.ranges = make(map[string]*trackedRange)
	t.haveLastSeqNo = false
	t.haveWindowStart = false
}

// event builds a TimelineEvent and resolves the discontinuity sequence at the
// event's program time.
func (t *EventTimeline) event(kind EventType, at time.Time, tr *trackedRange, clock segmentClock) TimelineEvent {
	ev := TimelineEvent{Type: kind, At: at, Range: tr.range_}
	if clock.hasProgramTime {
		ev.DiscontinuitySeq = clock.discontinuityAt(at)
		ev.DiscontinuityKnown = true
	}
	return ev
}

// explicitEnd resolves the end time declared on the range itself, either via
// END-DATE or via START-DATE plus DURATION. The boolean is false for an
// open-ended range.
func explicitEnd(dr *DateRange) (time.Time, bool) {
	if dr.EndDate != nil {
		return *dr.EndDate, true
	}
	if dr.Duration != nil {
		return dr.StartDate.Add(durationDuration(*dr.Duration)), true
	}
	return time.Time{}, false
}

// sortTimelineEvents orders events on the program clock. Events at the same
// time are ordered Start, Update, End so an interval that starts and ends at
// one instant is presented in lifecycle order, then by range ID.
func sortTimelineEvents(events []TimelineEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].At.Equal(events[j].At) {
			return events[i].At.Before(events[j].At)
		}
		if events[i].Type != events[j].Type {
			return events[i].Type < events[j].Type
		}
		return events[i].Range.ID < events[j].Range.ID
	})
}
