package m3u8

/* Package hls-m3u8 implements parsing and generation of HLS m3u8 playlists.

HLS (HTTP Live Streaming) is an evolving protocol with multiple versions.
Versions 1-7 are described in [IETF RFC8216][rfc8216], but the protocol has continued
to evolve with new features and versions in a
series of Internet Drafts [rfc8216bis].
The current version (Jan 3 2025) is [rfc8216bis-16].

One of the major libraries in Go for parsing and generating HLS playlists,
aka m3u8 files, has been the Github project [grafov/m3u8].
However, the majority of that code was written up to version 5,
It was finally archived in Dec. 2024.

The goal of this library, hls-m3u8,  is to provide an up-to-date replacement and improvement
of  the [grafov/m3u8] library. The aim is to follow the HLS specification
as it evolves and add all new elements and do other updates in order that
all m3u8 documents (from version 3 and forward) can be parsed and generated.

## Structure and design of the code

There are two types of m3u8 playlists: MasterPlaylist and MediaPlaylist.
These are represented as two different structs, but they have a common interface Playlist.

There is a function Decode, that decodes and autodetects the type of playlist by decoding
both in parallel, and stopping one, once the type is known.

For generating playlists, one starts by calling either NewMasterPlaylist or NewMediaPlaylist.
One can then Set or Append extra data such as Variants or Segments.

For live media playlists with a fixed sliding window, one can set a window size (winsize) that will
be used to Encode a maximum number of latest segments.

For VOD or EVENT media playlists, the winsize should be 0.

For writing, there are Encode methods that return a [*bytes.Buffer]. This buffer serves as a cache.

Library coded accordingly with IETF draft
http://tools.ietf.org/html/draft-pantos-http-live-streaming

Examples of usage may be found in *_test.go files.

There are also some simple examples below (without error handling).

Example 1: Create simple media playlist with sliding window of
  3 segments and maximum of 50 segments

	p, _ := NewMediaPlaylist(3, 50)
	for i := 0; i < 5; i++ {
	  _ = p.Append(fmt.Sprintf("test%d.ts", i), 5.0, "")
	}
	fmt.Println(p)

We add 5 testX.ts segments to the playlist before encoding it to M3U8 format and converting to string.
Due to the sliding window, only the last three
segments will be included.

Example 2: Parsing of a master playlist

	f, _ := os.Open("sample-playlists/master.m3u8")
	p := NewMasterPlaylist()
	_ = p.DecodeFrom(bufio.NewReader(f), true)
	fmt.Printf("Playlist object: %+v\n", p)

## Event timeline from EXT-X-DATERANGE

Live media playlists are reloaded on a timer, and their sliding window drops
old segments together with the EXT-X-DATERANGE tags that advertised ads or
chapters. The same tag can therefore be seen in several consecutive reloads,
but an application wants one Start, possibly some Updates and one End event per
range, on the program clock.

Keep one [EventTimeline] for the lifetime of a stream and feed it every
reloaded [MediaPlaylist] with [EventTimeline.Update]. It merges date ranges by
ID across reloads, preserves CLASS and the client-defined X- attributes,
resolves END-DATE, DURATION and END-ON-NEXT ends, and maps each event onto the
EXT-X-DISCONTINUITY-SEQUENCE of the media playing at its program time.

	timeline := m3u8.NewEventTimeline()
	// for each reloaded playlist:
	events, err := timeline.Update(mediaPlaylist)
	for _, ev := range events { // ev.Type is EventStart, EventUpdate or EventEnd
		fmt.Println(ev.Type, ev.At, ev.Range.ID)
	}

Ranges that have left the window stay tracked, so an ID never starts twice;
state is pruned once an ended range lies entirely before the window, and a
count bound keeps the state finite on very long-lived streams. With
[WithStrictTimeline], missing program dates, invalid intervals, conflicting
duplicate IDs and backwards-moving windows are returned wrapped in the Err*
sentinel errors; without it, the offending ranges are skipped instead.

[grafov/m3u8]: https://github.com/grafov/m3u8
[rfc8216]: https://tools.ietf.org/html/rfc8216
[rfc8216bis]: https://tools.ietf.org/html/draft-pantos-rfc8216bis
[rfc8216bis-16]: https://tools.ietf.org/html/draft-pantos-rfc8216bis-16
*/
