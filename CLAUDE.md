# wontexec

Claude Code skill and Go CLI that checks, without running anything, whether a Linux program will start inside a container image.

## Architecture
`plugin/scripts/wontexec.go` holds all the logic in one standard-library file, so `go run` needs no setup. `loadImage` reads an OCI layout, a docker save archive, a root filesystem tar or a directory into an in-memory file tree (`imageFS`), applying layers and whiteouts and checking digests and tar end blocks. `checkProgram` finds the program, then `checkFile` reads it as ELF and runs the architecture, loader, library, symbol version and Go checks. `plugin/skills/checking-container-start/SKILL.md` tells Claude how to use it. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet, unit tests and the integration tests in `tests/`
- `make lint`: plugin and marketplace manifest validation
- `make demo`: the three README examples, on images written by `examples/mkimage`

## Conventions
- Standard library only. Never run, extract to disk, or load anything being checked.
- An input it can't read fully (empty, cut off, digest mismatch, not a tar, not an ELF or script, an unreadable Go build info) exits 2 and prints no clean summary. Never skip it silently.
- Every check needs a real program showing the failure (run through the image's loader where possible), a test that flags it, and a healthy case that stays quiet.
- Tests must not assume the host GOOS or GOARCH. Cross-build with explicit targets; only the cgo tests use the host, and they skip without a C compiler.

## Do and don't
- Do keep README examples as real output from `make demo-pie`, `make demo-arch` and `make demo-fixed`.
- Don't print the Go version in the text output, so README output matches on any toolchain.
- Don't claim a program will start. The tool reports problems it found in the checks it ran.

## Definition of done
`make test`, `make lint` and `make demo` pass, and `ROADMAP.md`, `README.md` and docs are updated when relevant.
