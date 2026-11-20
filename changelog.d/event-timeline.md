### Added

- `EventTimeline` merges `EXT-X-DATERANGE` tags across media playlist reloads and emits
  Start, Update and End `TimelineEvent`s on the program clock via `NewEventTimeline` and
  `Update`, keeping `CLASS` and client-defined attributes and mapping events to
  `EXT-X-DISCONTINUITY-SEQUENCE` through `DiscontinuityAt`
