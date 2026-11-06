# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-06

### Added
- Reads OCI image layouts (tar or directory), docker save archives, root filesystem tars (plain or gzip) and root filesystem directories, with layers, whiteouts and opaque directories applied. Checks blob digests, layer `diff_ids` and tar end blocks, and refuses a partly copied OCI layout.
- Checks the entrypoint, `--exec` program or `--binary` program: present, on `PATH`, executable for the image user, and ELF for the image's architecture. Scripts: interpreter present, and CRLF line endings.
- Checks the ELF loader, the shared libraries on the loader's search path (glibc and musl), and glibc symbol versions.
- Checks the image user against `/etc/passwd` and `/etc/group`.
- For Go programs: CA certificates when the program loads system roots, and time zone data when it calls `time.LoadLocation` or the image sets `TZ`.
- `--platform`, `--user`, `--format json`, `--fail-on`, and exit 2 for any input that can't be read fully.
- `checking-container-start` skill and plugin manifest.
