# dmf-mf-mediamtx - working rules

A fork of bluenviron/mediamtx carrying one addition: the `mxl://` static
source. The only build output is a container image.

## Voice

- Terse. Say the thing, stop. No preamble, no recap, no restating the task.
- No filler adjectives (robust, seamless, powerful, comprehensive). State what
  the code does, not how good it is.
- Comments explain *why*, not *what*. Delete comments that restate the code.
  Revalidate a comment against its context before leaving it in place; remove
  stale references and accounts of how the code came to look this way.
- Write declarative facts. No personal pronouns. Don't address a reader.
- Commit messages: conventional-commit, imperative, subject <= 72 chars, body
  wrapped at 72. Breaking changes get `!` or a `BREAKING CHANGE:` footer.
- No ticket numbers in code, commits or docs. No checklists, "Summary" or
  "Test plan" sections, no emojis, no `Co-Authored-By` trailers.
- ASCII only: `-`, `--`, `"`, `'`, `->`, `...`. No em-dash, no typographic
  quotes.

## Worktree

Every mutation happens in a git worktree under `.claude/worktrees/`:

    git fetch origin
    id=$(openssl rand -hex 4)
    git worktree add .claude/worktrees/<topic>-$id -b <topic> origin/main

The main checkout is for reading only. Several sessions work this tree at
once, and two writers in one working tree lose each other's work.

## Fork boundaries

- `main` carries the fork. `upstream-main` tracks bluenviron/mediamtx
  unmodified and is the base for merging upstream in.
- The fork adds; it does not rewrite upstream logic. Keep the delta confined
  to the MXL source and the configuration fields that reach it, so an upstream
  merge stays mechanical.
- Upstream's release, binary and Docker Hub workflows were removed. Do not
  reintroduce them from an upstream merge: they publish under upstream's
  identity and comment on issues by number.
- No Helm chart lives here. The chart that deploys this image is authored
  where the media-function catalog lives.

## Building

The MXL source is cgo against libmxl, so `go build`, `go test ./...` and
golangci-lint all fail on a host without libmxl and `pkg-config`.
`Dockerfile.mxl` is the reference build; it uses `go-mxl-builder`, which ships
libmxl under `/opt/libmxl`.

Every reader and writer sharing an MXL domain must link a byte-identical
libmxl. `ARG GO_MXL_TAG` pins it, and moving it in isolation from the node
agent and the writers corrupts cross-node reads without an error.

## Known-failing checks

`test.yml` and `lint.yml` are inherited from upstream and fail on the fork:
the go2api linter compares reflected `conf.Path` against `api/openapi.yaml`,
which does not list the `mxl*` properties; the `internal/conf` fixture does
not know the fork's defaults; and neither runner has libmxl. Fixing them means
either teaching them about the fork or scoping them to what can pass.
