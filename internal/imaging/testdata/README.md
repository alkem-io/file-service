# Test fixtures

Real-world image fixtures for the streaming-transcode tests (spec 020).

- `heic-*`, `jpg-*`, `png-*`, `webp-*`, `gif-*`, `svg.svg`, `avif-*`, `bmp.bmp`
  are copied from the govips test corpus
  (github.com/davidbyttow/govips, MIT license) — the same corpus the
  streaming library itself is validated against. (They were originally
  taken from the antst/govips fork, whose streaming work has since been
  upstreamed; the corpus is unchanged.)
- `pixel-bomb-30000x30000.png` is generated locally (see git history): a
  74-byte PNG whose header declares 900 MP (~3.6 GB decoded RGBA). Exercises
  the FR-010 pixel budget — must be rejected from header metadata before any
  pixel decode.
- `generate_test.go` (spec 018) generates synthetic orientation fixtures.

Notable cases: `jpg-orientation-6.jpg` (real 3 MB photo, EXIF rotation →
materialized path), `heic-orientation-6.heic` (whole-frame codec + rotation),
`jpg-corruption.jpg` (decoder failure propagation), `webp+alpha.webp`
(format conversion), `gif-animated.gif`/`svg.svg` (pass-through formats).
