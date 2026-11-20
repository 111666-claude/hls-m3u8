package m3u8

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/matryer/is"
)

// decodeTimelineSample decodes one fixed refresh playlist.
func decodeTimelineSample(t *testing.T, name string) *MediaPlaylist {
	t.Helper()
	is := is.New(t)
	f, err := os.Open("sample-playlists/" + name)
	is.NoErr(err) // open sample playlist should succeed
	defer f.Close()
	p, _, err := DecodeFrom(bufio.NewReader(f), true)
	is.NoErr(err) // decode sample playlist should succeed
	return p.(*MediaPlaylist)
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("bad test time %q: %v", value, err)
	}
	return at
}

// TestEventTimelineThreeRefreshes drives one EventTimeline through the three
// fixed reloads of a sliding-window live playlist. The ad crosses an
// EXT-X-DISCONTINUITY and gains its end in a later reload, while the window
// repeatedly drops segments and tags.
func TestEventTimelineThreeRefreshes(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline()

	// Refresh 1: chapter-1 starts and ends, ad-1 starts open-ended across the
	// discontinuity between seg-101 and seg-102.
	events1, err := tl.Update(decodeTimelineSample(t, "timeline-refresh-1.m3u8"))
	is.NoErr(err) // first refresh should not fail
	is.Equal(tl.RefreshCount(), uint64(1))
	is.Equal(len(events1), 3) // chapter-1 start+end and ad-1 start

	want1 := []struct {
		typ   EventType
		at    string
		id    string
		disco uint64
		known bool
	}{
		{EventStart, "2026-11-20T10:00:00Z", "chapter-1", 1, true},
		{EventStart, "2026-11-20T10:00:06Z", "ad-1", 1, true},
		{EventEnd, "2026-11-20T10:00:12Z", "chapter-1", 2, true},
	}
	for i, want := range want1 {
		is.Equal(events1[i].Type, want.typ)
		is.Equal(events1[i].At, mustTime(t, want.at))
		is.Equal(events1[i].Range.ID, want.id)
		is.Equal(events1[i].DiscontinuityKnown, want.known)
		is.Equal(events1[i].DiscontinuitySeq, want.disco)
	}
	// Class and client-defined X- attributes must survive into the event.
	adStart := events1[1].Range
	is.Equal(adStart.Class, "com.example.ad")
	is.Equal(len(adStart.XAttrs), 2) // X-CAMPAIGN and X-URI
	xattrs := xAttrsAsMap(adStart)
	is.Equal(xattrs["X-CAMPAIGN"], `"fallback-2026"`)
	is.Equal(xattrs["X-URI"], `"http://example.com/ad1.m3u8"`)

	// Refresh 2: the ad reappears with a DURATION, so it must not start again;
	// it emits one Update followed by one End. chapter-2 starts and ends.
	events2, err := tl.Update(decodeTimelineSample(t, "timeline-refresh-2.m3u8"))
	is.NoErr(err)             // second refresh should not fail
	is.Equal(len(events2), 4) // ad update+end, chapter-2 start+end
	is.Equal(events2[0].Type, EventUpdate)
	is.Equal(events2[0].Range.ID, "ad-1")
	is.Equal(events2[1].Type, EventStart)
	is.Equal(events2[1].Range.ID, "chapter-2")
	is.Equal(events2[2].Type, EventEnd)
	is.Equal(events2[2].Range.ID, "ad-1")
	is.Equal(events2[2].At, mustTime(t, "2026-11-20T10:00:18Z")) // 10:00:06 + 12s
	is.Equal(events2[3].Type, EventEnd)
	is.Equal(events2[3].Range.ID, "chapter-2")
	// The update carries the merged range, keeping class and X- attributes.
	is.Equal(events2[0].Range.Class, "com.example.ad")
	is.Equal(len(events2[0].Range.XAttrs), 2)
	is.Equal(events2[0].Range.Duration != nil, true)

	// Refresh 3: segments 100 and 101 have left the window together with
	// chapter-1 and ad-1, which must not be replayed. Only chapter-3 is new.
	events3, err := tl.Update(decodeTimelineSample(t, "timeline-refresh-3.m3u8"))
	is.NoErr(err)             // third refresh should not fail
	is.Equal(len(events3), 2) // chapter-3 start+end
	is.Equal(events3[0].Type, EventStart)
	is.Equal(events3[0].Range.ID, "chapter-3")
	is.Equal(events3[0].DiscontinuitySeq, uint64(3)) // first segment is post-discontinuity
	is.Equal(events3[1].Type, EventEnd)
	is.Equal(events3[1].At, mustTime(t, "2026-11-20T10:00:36Z"))

	// Ranges that ended before the window are pruned, so a reappearing ID would
	// be a genuine new range rather than a duplicate start.
	is.Equal(len(tl.ActiveRanges()), 0)
}

// TestEventTimelineIdempotentReload checks that reloading an unchanged
// playlist emits nothing: this is the "do not pop the same ad every refresh"
// guarantee.
func TestEventTimelineIdempotentReload(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline()
	p := decodeTimelineSample(t, "timeline-refresh-2.m3u8")

	first, err := tl.Update(p)
	is.NoErr(err) // first refresh should not fail
	if len(first) == 0 {
		t.Fatal("first refresh should produce events")
	}
	second, err := tl.Update(p)
	is.NoErr(err)            // identical refresh should not fail
	is.Equal(len(second), 0) // unchanged ranges must not emit events
}

// TestEventTimelineSlidingWindowRetention verifies state is kept after a range
// leaves the window: an open-ended ad that is updated only after its start tag
// has slid out still emits Update and End, never a second Start.
func TestEventTimelineSlidingWindowRetention(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline()

	first := playlistWithRanges(t, 100, []*DateRange{
		{
			ID:        "ad-open",
			Class:     "com.example.ad",
			StartDate: mustTime(t, "2026-11-20T10:00:06Z"),
			XAttrs:    []Attribute{{Key: "X-CAMPAIGN", Val: `"c1"`}},
		},
	})
	events, err := tl.Update(first)
	is.NoErr(err) // first refresh should not fail
	is.Equal(len(events), 1)
	is.Equal(events[0].Type, EventStart)

	// Next window no longer carries the tag at all.
	empty := playlistWithRanges(t, 104, nil)
	events, err = tl.Update(empty)
	is.NoErr(err)            // window without the tag should not fail
	is.Equal(len(events), 0) // the range is still tracked, not restarted

	// The closing observation arrives two reloads later with an END-DATE.
	dur := 12.0
	closing := playlistWithRanges(t, 108, []*DateRange{
		{
			ID:        "ad-open",
			Class:     "com.example.ad",
			StartDate: mustTime(t, "2026-11-20T10:00:06Z"),
			Duration:  &dur,
			XAttrs:    []Attribute{{Key: "X-CAMPAIGN", Val: `"c1"`}},
		},
	})
	events, err = tl.Update(closing)
	is.NoErr(err)            // closing refresh should not fail
	is.Equal(len(events), 2) // update then end, no start
	is.Equal(events[0].Type, EventUpdate)
	is.Equal(events[1].Type, EventEnd)
	is.Equal(events[1].At, mustTime(t, "2026-11-20T10:00:18Z"))
}

// TestEventTimelineEndOnNext checks the END-ON-NEXT resolution of
// rfc8216bis 4.4.5.1.2: the predecessor ends where the next range of the same
// class begins.
func TestEventTimelineEndOnNext(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline()
	p := playlistWithRanges(t, 0, []*DateRange{
		{
			ID:        "ad-a",
			Class:     "com.example.ad",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			EndOnNext: true,
		},
		{
			ID:        "ad-b",
			Class:     "com.example.ad",
			StartDate: mustTime(t, "2026-11-20T10:00:30Z"),
		},
	})
	events, err := tl.Update(p)
	is.NoErr(err)            // END-ON-NEXT refresh should not fail
	is.Equal(len(events), 3) // start a, start b, end a at b's start
	is.Equal(events[0].Type, EventStart)
	is.Equal(events[0].Range.ID, "ad-a")
	is.Equal(events[1].Type, EventStart)
	is.Equal(events[1].Range.ID, "ad-b")
	is.Equal(events[2].Type, EventEnd)
	is.Equal(events[2].Range.ID, "ad-a")
	is.Equal(events[2].At, mustTime(t, "2026-11-20T10:00:30Z"))
}

// TestEventTimelineStrictErrors covers the validation rules of requirement 3.
func TestEventTimelineStrictErrors(t *testing.T) {
	t.Run("missing program date on playlist", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		p, _ := NewMediaPlaylist(0, 4)
		is.NoErr(p.Append("a.ts", 6, ""))
		p.DateRanges = []*DateRange{{
			ID:        "r1",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
		}}
		_, err := tl.Update(p)
		is.Equal(errors.Is(err, ErrMissingProgramDate), true)
	})

	t.Run("date range without start date", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		p := playlistWithRanges(t, 0, []*DateRange{{ID: "no-start"}})
		_, err := tl.Update(p)
		is.Equal(errors.Is(err, ErrMissingProgramDate), true)
	})

	t.Run("invalid interval", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		p := playlistWithRanges(t, 0, []*DateRange{{
			ID:        "bad",
			StartDate: mustTime(t, "2026-11-20T10:00:10Z"),
			EndDate:   timePointer(mustTime(t, "2026-11-20T10:00:00Z")),
		}})
		_, err := tl.Update(p)
		is.Equal(errors.Is(err, ErrInvalidDateRangeInterval), true)
	})

	t.Run("duplicate id conflict in one playlist", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		p := playlistWithRanges(t, 0, []*DateRange{
			{
				ID:        "dup",
				Class:     "com.example.ad",
				StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			},
			{
				ID:        "dup",
				Class:     "com.example.chapter",
				StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			},
		})
		_, err := tl.Update(p)
		is.Equal(errors.Is(err, ErrDuplicateDateRangeID), true)
	})

	t.Run("immutable attribute changes across reloads", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		first := playlistWithRanges(t, 0, []*DateRange{{
			ID:        "r",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			EndDate:   timePointer(mustTime(t, "2026-11-20T10:00:10Z")),
		}})
		_, err := tl.Update(first)
		is.NoErr(err) // first observation should be accepted
		second := playlistWithRanges(t, 1, []*DateRange{{
			ID:        "r",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			EndDate:   timePointer(mustTime(t, "2026-11-20T10:00:11Z")),
		}})
		_, err = tl.Update(second)
		is.Equal(errors.Is(err, ErrDuplicateDateRangeID), true)
	})

	t.Run("media sequence goes backwards", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		_, err := tl.Update(playlistWithRanges(t, 10, nil))
		is.NoErr(err) // forward reload should be accepted
		_, err = tl.Update(playlistWithRanges(t, 9, nil))
		is.Equal(errors.Is(err, ErrDateRangeOutOfOrder), true)
	})

	t.Run("failed strict update changes no state", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		good := playlistWithRanges(t, 0, []*DateRange{{
			ID:        "keep",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
		}})
		_, err := tl.Update(good)
		is.NoErr(err) // good reload should be accepted
		bad := playlistWithRanges(t, 1, []*DateRange{{
			ID:        "bad",
			StartDate: mustTime(t, "2026-11-20T10:00:10Z"),
			EndDate:   timePointer(mustTime(t, "2026-11-20T10:00:00Z")),
		}})
		_, err = tl.Update(bad)
		is.Equal(err != nil, true) // invalid interval must be rejected
		// The good range is still tracked and the failed one never registered.
		active := tl.ActiveRanges()
		is.Equal(len(active), 1)
		is.Equal(active[0].ID, "keep")
		// The clock of the rejected playlist must not have replaced the good one.
		disco, known := tl.DiscontinuityAt(mustTime(t, "2026-11-20T10:00:03Z"))
		is.Equal(known, true)
		is.Equal(disco, uint64(0))
	})
}

// TestEventTimelineLenientRecovery checks that a non-strict timeline skips
// invalid ranges and resets on a backwards-moving window instead of failing.
func TestEventTimelineLenientRecovery(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline()
	p := playlistWithRanges(t, 5, []*DateRange{
		{ID: "no-start"}, // skipped: no START-DATE
		{
			ID:        "bad-interval",
			StartDate: mustTime(t, "2026-11-20T10:00:40Z"),
			EndDate:   timePointer(mustTime(t, "2026-11-20T10:00:00Z")),
		}, // skipped: invalid interval
		{
			ID:        "good",
			StartDate: mustTime(t, "2026-11-20T10:00:30Z"),
		},
	})
	events, err := tl.Update(p)
	is.NoErr(err) // lenient mode must not return validation errors
	is.Equal(len(events), 1)
	is.Equal(events[0].Range.ID, "good")

	// A backwards window resets tracking, so the same range starts once more
	// rather than being silently merged across a discontinuous stream.
	backwards := playlistWithRanges(t, 4, []*DateRange{
		{ID: "good", StartDate: mustTime(t, "2026-11-20T10:00:24Z")},
	})
	events, err = tl.Update(backwards)
	is.NoErr(err) // backwards reload must not fail in lenient mode
	is.Equal(len(events), 1)
	is.Equal(events[0].Type, EventStart)
}

// TestEventTimelineBoundedState proves the timeline cannot grow without limit
// on a long-lived live stream, including when no program clock is available.
func TestEventTimelineBoundedState(t *testing.T) {
	is := is.New(t)
	const limit = 5
	tl := NewEventTimeline(WithMaxTrackedRanges(limit))
	base := mustTime(t, "2026-11-20T10:00:00Z")
	for seq := uint64(0); seq < 20; seq++ {
		// No PDT anywhere: program-time pruning is disabled, only the bound
		// keeps the state finite.
		p, _ := NewMediaPlaylist(0, 1)
		p.SeqNo = seq
		is.NoErr(p.Append("seg.ts", 6, ""))
		p.DateRanges = []*DateRange{{
			ID:        "r" + time.Duration(seq).String(),
			StartDate: base.Add(time.Duration(seq) * time.Second),
		}}
		_, err := tl.Update(p)
		is.NoErr(err) // each bounded refresh should succeed
		if len(tl.ranges) > limit {
			t.Fatalf("tracked %d ranges, bound is %d", len(tl.ranges), limit)
		}
	}
	is.Equal(len(tl.ranges), limit) // the bound holds
}

// TestEventTimelineCollectsSCTE35Ranges makes sure the timeline also consumes
// EXT-X-DATERANGE tags stored per segment (the SCTE-35 syntax), not only header
// and trailing tags.
func TestEventTimelineCollectsSCTE35Ranges(t *testing.T) {
	is := is.New(t)
	mp := decodeTimelineSample(t, "media-playlist-with-scte35-daterange.m3u8")

	// Strict mode rejects the sample's same-ID tags with different START-DATEs
	// as the specification requires.
	strict := NewEventTimeline(WithStrictTimeline())
	_, err := strict.Update(mp)
	is.Equal(errors.Is(err, ErrDuplicateDateRangeID), true)

	// In lenient mode the conflicting second observation is skipped, leaving
	// the first range, which ends via its DURATION.
	tl := NewEventTimeline()
	events, err := tl.Update(mp)
	is.NoErr(err)            // lenient collection should not fail
	is.Equal(len(events), 2) // one start, one end from DURATION
	is.Equal(events[0].Type, EventStart)
	is.Equal(events[0].Range.SCTE35Out != "", true) // SCTE-35 attribute is preserved
	is.Equal(events[1].Type, EventEnd)
	is.Equal(events[1].At, mustTime(t, "2014-03-05T11:16:00Z"))
}

// TestEventTimelineMergesDuplicateIDs checks that two same-ID tags in one
// playlist merge when they only carry complementary attributes, which is the
// legal updated-range form of rfc8216bis 4.4.5.1.1.
func TestEventTimelineMergesDuplicateIDs(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline(WithStrictTimeline())
	p := playlistWithRanges(t, 0, []*DateRange{
		{
			ID:        "r",
			Class:     "com.example.ad",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			XAttrs:    []Attribute{{Key: "X-A", Val: `"1"`}},
		},
		{
			ID:        "r",
			Class:     "com.example.ad",
			StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
			Duration:  floatPointer(10),
			XAttrs:    []Attribute{{Key: "X-B", Val: `"2"`}},
		},
	})
	events, err := tl.Update(p)
	is.NoErr(err)            // complementary duplicates must merge, not conflict
	is.Equal(len(events), 2) // one start and one end from the merged DURATION
	is.Equal(events[0].Type, EventStart)
	merged := xAttrsAsMap(events[0].Range)
	is.Equal(merged["X-A"], `"1"`) // both client attributes survive the merge
	is.Equal(merged["X-B"], `"2"`)
}

// TestEventTimelineOutOfOrderRanges covers START-DATEs that go backwards
// within one playlist: strict mode rejects them, lenient mode sorts the
// resulting events onto the program clock.
func TestEventTimelineOutOfOrderRanges(t *testing.T) {
	ranges := []*DateRange{
		{ID: "later", StartDate: mustTime(t, "2026-11-20T10:00:12Z")},
		{ID: "earlier", StartDate: mustTime(t, "2026-11-20T10:00:00Z")},
	}

	t.Run("strict rejects", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline(WithStrictTimeline())
		_, err := tl.Update(playlistWithRanges(t, 0, ranges))
		is.Equal(errors.Is(err, ErrDateRangeOutOfOrder), true)
	})

	t.Run("lenient sorts", func(t *testing.T) {
		is := is.New(t)
		tl := NewEventTimeline()
		events, err := tl.Update(playlistWithRanges(t, 0, ranges))
		is.NoErr(err) // lenient mode must not reject the order
		is.Equal(len(events), 2)
		is.Equal(events[0].Range.ID, "earlier") // program-time order, not playlist order
		is.Equal(events[1].Range.ID, "later")
	})
}

// TestEventTimelineNoProgramClock keeps working without PDT: events are still
// emitted on absolute START-DATEs, only discontinuity mapping is unavailable.
func TestEventTimelineNoProgramClock(t *testing.T) {
	is := is.New(t)
	tl := NewEventTimeline()
	p, _ := NewMediaPlaylist(0, 2)
	p.DiscontinuitySeq = 4
	is.NoErr(p.Append("a.ts", 6, ""))
	is.NoErr(p.Append("b.ts", 6, ""))
	p.DateRanges = []*DateRange{{
		ID:        "r",
		Class:     "com.example.chapter",
		StartDate: mustTime(t, "2026-11-20T10:00:00Z"),
	}}
	events, err := tl.Update(p)
	is.NoErr(err) // playlist without PDT should not fail in lenient mode
	is.Equal(len(events), 1)
	is.Equal(events[0].DiscontinuityKnown, false)
	_, ok := tl.DiscontinuityAt(mustTime(t, "2026-11-20T10:00:00Z"))
	is.Equal(ok, false)
}

// playlistWithRanges builds a one-segment media playlist with a PDT anchor and
// the supplied header date ranges, so the event logic can be tested without
// hand-writing m3u8 text.
func playlistWithRanges(t *testing.T, seq uint64, ranges []*DateRange) *MediaPlaylist {
	t.Helper()
	is := is.New(t)
	p, err := NewMediaPlaylist(0, 1)
	is.NoErr(err) // constructing the playlist should succeed
	p.SeqNo = seq
	is.NoErr(p.Append("seg.ts", 6, ""))
	is.NoErr(p.SetProgramDateTime(mustTime(t, "2026-11-20T10:00:00Z").
		Add(time.Duration(seq) * 6 * time.Second)))
	p.DateRanges = ranges
	return p
}

func timePointer(at time.Time) *time.Time {
	return &at
}

func floatPointer(v float64) *float64 {
	return &v
}

// ExampleEventTimeline keeps one timeline across three fixed reloads of a live
// playlist and prints the Start, Update and End events. The ad ad-1 appears in
// every reload but starts only once, and its end is delivered when a later
// reload adds its DURATION.
func ExampleEventTimeline() {
	timeline := NewEventTimeline()
	for refresh := 1; refresh <= 3; refresh++ {
		name := fmt.Sprintf("sample-playlists/timeline-refresh-%d.m3u8", refresh)
		f, _ := os.Open(name)
		playlist, _, _ := DecodeFrom(bufio.NewReader(f), true)
		f.Close()
		events, _ := timeline.Update(playlist.(*MediaPlaylist))
		for _, ev := range events {
			fmt.Printf("refresh %d: %s %s %s discontinuity=%d\n",
				refresh, ev.Type, ev.At.UTC().Format(time.RFC3339), ev.Range.ID, ev.DiscontinuitySeq)
		}
	}
	// Output:
	// refresh 1: start 2026-11-20T10:00:00Z chapter-1 discontinuity=1
	// refresh 1: start 2026-11-20T10:00:06Z ad-1 discontinuity=1
	// refresh 1: end 2026-11-20T10:00:12Z chapter-1 discontinuity=2
	// refresh 2: update 2026-11-20T10:00:06Z ad-1 discontinuity=1
	// refresh 2: start 2026-11-20T10:00:12Z chapter-2 discontinuity=2
	// refresh 2: end 2026-11-20T10:00:18Z ad-1 discontinuity=2
	// refresh 2: end 2026-11-20T10:00:30Z chapter-2 discontinuity=2
	// refresh 3: start 2026-11-20T10:00:30Z chapter-3 discontinuity=3
	// refresh 3: end 2026-11-20T10:00:36Z chapter-3 discontinuity=3
}
