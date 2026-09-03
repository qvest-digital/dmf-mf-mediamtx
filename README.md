# dmf-mf-mediamtx

A fork of [bluenviron/mediamtx](https://github.com/bluenviron/mediamtx) that
adds an MXL static source. Not an official MediaMTX release.

Upstream documentation applies to everything else this server does:
[mediamtx.org](https://mediamtx.org/docs/kickoff/introduction).

## What the fork adds

A `mxl://` static source. A path configured with
`source: mxl:///<runtime-root>/<domain>/<flow-id>` opens that MXL flow through
libmxl and republishes it over the server's existing RTSP, HLS, WebRTC and SRT
outputs. Grains are read zero-copy from the node-local domain, and the reader
is paced to the flow's nominal grain rate rather than polling the ring buffer
at maximum speed.

Encoding shells out to `ffmpeg`, so no GPL-licensed code is linked into the
binary.

Path options the fork adds, accepted by both the configuration file and the
`/v3/config/paths/*` HTTP API: `mxlFFmpegPath`, `mxlCodec`, `mxlH264Preset`,
`mxlH264Profile`, `mxlH264Bitrate`, `mxlH264IDRPeriod`.

Geometry and frame rate come from the flow, never from configuration. The
encoder is given the rate as the fraction the flow declares, so a 60000/1001
flow is encoded at 59.94 rather than at a rounded 60. `mxlH264IDRPeriod`
counts frames and defaults to half a second of them at that rate; a GOP the
length of `hlsSegmentDuration` sits on the HLS muxer's cut comparison, which
makes segments alternate between one and two of them.

x264's frame threading is fixed at four threads and is not configurable. Left
to itself it takes one and a half threads per host core and holds a frame per
thread before the first access unit comes back, which is 818 ms of added
latency at 1080p59.94 on a 32-core node and more frames than the source will
queue on a 128-core one.

Paths can be added and removed at runtime over the HTTP API with none present
at boot; a newly added `mxl://` path starts its source immediately.

## Image

    ghcr.io/qvest-digital/dmf-mf-mediamtx

Tags: the 7-character commit SHA for every build on `main`, `latest` for the
head of `main`, and the bare version for an `mxl-v*` tag.

## Building

The MXL source is cgo and links libmxl, so a plain `go build` needs
`pkg-config` and libmxl present and fails without them. `Dockerfile.mxl`
builds against `go-mxl-builder`, which ships libmxl under `/opt/libmxl`.

Every reader and writer sharing an MXL domain must link a byte-identical
libmxl. `ARG GO_MXL_TAG` in `Dockerfile.mxl` is what pins it.

## Branches

- `main` carries the fork.
- `upstream-main` tracks `bluenviron/mediamtx` unmodified and is the base for
  merging upstream changes.

## License

MIT, unchanged from upstream. See [LICENSE](LICENSE).
