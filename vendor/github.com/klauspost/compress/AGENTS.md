# AGENTS.md

Guidance for people and coding agents changing this repository. It records
conventions and traps learned while working on the decoders and encoders; the
README describes the packages themselves.

## Checks before sending a change

- `gofmt`, `go vet ./...` and `go fix -diff ./...` (which should report
  nothing). Code written for specific platforms must be vetted for them too:
  run `go vet` with `GOOS`/`GOARCH` set to each platform it targets. `go vet`
  caches results per package and does not notice edits to files excluded by
  build tags; use a fresh `GOCACHE` to confirm a stale-looking error.
- `go test ./...` with and without `-tags=noasm` (and `nounsafe` if you
  touched `unsafe` code), on every architecture whose assembly changed.
- Decoder changes: run the fuzzers CI runs (see `.github/workflows/go.yml`),
  also with `-tags=noasm`, `-tags=nounsafe` and `-asan`, for longer than CI
  does, and for zstd both `FuzzDecodeAll` and the `NoBMI2` variants.
- Raising the Go version in the root `go.mod` breaks the nested modules
  (`*/_generate`, `s2/cmd/_s2sx`) until they are updated too. In a `go`
  directive, `1.25` sorts before `1.25.0`, so a dependency that needs
  `1.25.0` forces the full form.

## Tests

- Put a new test in the existing test file for the code it exercises
  (`decoder_test.go` for Reader/DecodeAll behaviour, `seqdec_test.go` for
  sequence execution, and so on), and reuse its helpers. Use table tests for
  three or more similar cases.
- Extend the existing fuzzers (`func Fuzz…`) rather than adding parallel
  harnesses; if a new harness supersedes an old one, remove the old one in
  the same change.
- Prove which path a test or benchmark exercises. A benchmark once ran the
  pure-Go sequence decoder instead of the assembly because of how its buffers
  were set up. A `panic` planted in the path is a quick check; `println`
  output is easy to miss.
- `sync.Pool` drops about a quarter of `Put`s under `-race` by design; never
  assume `Get` returns the last `Put`.
- Don't add tests whose only purpose is to catch someone deliberately undoing
  a performance pin (an alignment, say); a comment explaining it is enough.

## Performance claims

- Use real data. The maintainer's corpora are at
  https://klauspost.com/files/compress/ (logs, JSON, CSV, serialized data,
  tar files); put `.zst` files in `zstd/testdata/benchmark-custom/` and run
  `BenchmarkDecoderWithCustomFiles`. Silesia, enwik and generated data are
  fine for correctness but are not accepted as evidence for a speed-up.
- When a result depends on how the data was compressed, report the levels
  separately (for example `SpeedDefault` and `SpeedBestCompression`).
- Anchor `-bench` patterns at every level with `$`: `-bench 'BenchmarkFoo$'`
  also runs `BenchmarkFooBar` otherwise, which has produced fake gains and
  regressions.
- Code placement alone moves results by about ±2%. For an A/B, insert
  `PCALIGN $64` before every `TEXT` in the `.s` files of *both* builds
  (measurement only, not committed).
- Build both variants as test binaries up front, then run them in turns: one
  short `-count 1` run of each, repeated for ten or more rounds, swapping
  which goes first every round (AB, BA, AB, …) so neither build always gets
  the warmer or colder slot. Never run all of A and then all of B. Clock
  speed, temperature, cache and page-cache state, and background load drift
  over a session; alternating spreads that drift over both builds instead of
  turning it into a difference between them. Pin to one core, use an
  otherwise idle machine, and compare the pooled results with `benchstat`.
- Report the number of regressed cases next to the geomean; a small win with
  no regressions is a different result from a larger one with some.
- No CPU-model-specific code paths or thresholds. A gate for arm64 must be
  measured on several Neoverse generations (N1, V1, V2, V3), not one core.

## Pull requests

- One commit per independent change, with fixups squashed in. Number the
  changes in the description as "(1) … (sha)" and name the test file for
  each test.
- Mention each `#NNN` once in prose; refer back in words.
- Don't open many PRs against the same area at once; reviewer time is the
  scarce resource.
- Keep Go comments short; generator and assembly comments can explain in
  detail.
- Keep private or company-internal references (internal service names,
  private links, chat or agent-session URLs) out of code, commit messages and
  PR text.

## Assembly is generated

- zstd, huff0 and s2 assembly is generated with [avo](https://github.com/mmcloughlin/avo)
  from `zstd/_generate`, `huff0/_generate` and `s2/_generate`. Never edit the
  `.s` files by hand: change the generator, run `go generate -v -x` in the
  `_generate` directory, and commit the regenerated output. CI's `generate` job
  fails if the committed files differ from the generator's output.
- The arm64 files are lowered from the same amd64 program by the
  `honeycombio/avo` fork that the `_generate/go.mod` files `replace` avo with.
  A change to the generator changes both architectures; build and test both.
- When moving the avo pin, regenerate and check the output is byte-identical
  unless a change is intended, so unrelated printer changes don't slip into a
  review.
- Never put a label directly on a `PCALIGN` (emit the `PCALIGN` before the
  label). A jump to it makes the amd64 assembler jump to the wrong place
  (Go ≤ 1.25, golang/go#74648) or loop forever when it has to widen a branch
  (Go ≥ 1.26, golang/go#81792). Current versions of the avo fork reject it.
- Build tags: `noasm` selects the pure-Go code and `nounsafe` the code without
  `unsafe`. Every assembly or `unsafe` path needs a portable fallback, and CI
  runs the default, `noasm`, `nounsafe` and `nounsafe,noasm` builds. The
  library doesn't use cgo.
