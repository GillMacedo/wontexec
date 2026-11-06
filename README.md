# wontexec: check that a program will start in a container image

[![CI](https://github.com/GillMacedo/wontexec/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/wontexec/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI that finds why a container won't start, before you deploy: `exec /app: no such file or directory` from a missing loader, `exec format error` from the wrong CPU architecture, missing shared libraries, `version GLIBC_2.38 not found`, an image user that isn't in `/etc/passwd`, and for Go programs missing CA certificates or time zone data.**

These failures show up as a pod in CrashLoopBackOff right after a base image change, a move to scratch, distroless or Alpine, or a new build flag. The usual way to find them is to deploy and read the logs. `wontexec` reads the image archive (a `docker save` tar, an OCI layout or a root filesystem) and the program as data, and follows the same steps the kernel and the loader would. It never runs the image or the program, so it works in CI and on a laptop without Docker, and an arm64 Mac gives the same answer about an amd64 image.

## What you can ask Claude

- "Our pod is in CrashLoopBackOff with exec /app/server: no such file or directory, but the file is in the image. Why?"
- "Will this Go service still start if I move it from debian:bookworm-slim to distroless/static?"
- "The new image fails with version GLIBC_2.34 not found. Which base image do we need?"
- "Check that the image we just built for arm64 really runs on arm64."

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/wontexec
    claude plugin install wontexec@wontexec-marketplace

Or run it directly with Go 1.22 or newer. It has no dependencies:

    go run github.com/GillMacedo/wontexec/plugin/scripts@latest image.tar

## Usage

Get the image as a file first, with whatever you already use: `docker save IMAGE -o image.tar`, `podman save`, `skopeo copy docker://IMAGE oci-archive:image.tar`, `crane pull IMAGE image.tar`, or a root filesystem from `docker export` or BuildKit's local output. Then:

    wontexec image.tar                                    # the image's own entrypoint
    wontexec --binary ./bin/app --at /app base.tar        # a program built here, as if copied into a base image
    wontexec --exec /usr/local/bin/worker image.tar       # another program in the image
    wontexec --platform linux/arm64 --user 65532 image.tar

`--format json` gives machine-readable output. `--fail-on` sets the lowest level that exits 1: `warn` (the default), `error` or `none`.

Exit 0 means no findings at the `--fail-on` level. Exit 1 means findings. Exit 2 means bad usage, or an input that can't be read fully: an empty file, a garbage file, an archive cut off partway, a layer whose digest doesn't match, a partly copied OCI layout, a program that isn't an ELF file or a script, or the saved output of a failed build. Exit 2 ignores `--fail-on` and prints no summary, so an unreadable input can't pass.

## Examples

These use a made-up Go service, `examples/hello`, and two small images written by `examples/mkimage`. The `make` targets build everything for linux/amd64 or linux/arm64, so the output is the same on any machine. See [examples/README.md](examples/README.md).

A `-buildmode=pie` build copied into an empty (scratch) image. It is built with `CGO_ENABLED=0`, and still asks for a loader:

    $ make demo-pie
    image:   bin/scratch-amd64.tar (OCI layout, linux/amd64, 1 layer)
    program: /app (--binary bin/hello-pie), Go program, CGO_ENABLED=0
      ok    user            runs as root (the image sets no user)
      ok    permission      /app is executable (mode 0755)
      ok    architecture    /app is amd64, the same as the image
      ERROR loader          /app needs the loader /lib64/ld-linux-x86-64.so.2, which is not in the image
                            exec fails with: exec /app: no such file or directory (the message means the loader, not the program)
                            this is a -buildmode=pie build, which asks for a loader even with CGO_ENABLED=0; build without -buildmode=pie, or use a base image with a C library such as distroless/base
      WARN  ca-certificates /app loads the system CA certificates, and the image has none
                            outgoing TLS connections fail with: tls: failed to verify certificate: x509: certificate signed by unknown authority
                            copy /etc/ssl/certs/ca-certificates.crt from the build stage, use a base image with certificates such as distroless/static, or import golang.org/x/crypto/x509roots/fallback
      WARN  time-zones      /app calls time.LoadLocation, and the image has no time zone data
                            loading a named zone fails with: unknown time zone Europe/Berlin (any name other than "UTC" and "Local")
                            install tzdata in the image, or build with -tags timetzdata or import _ "time/tzdata"
      1 error, 2 warnings: this program won't start in this image as it is.

An arm64 build in an amd64 image that has certificates, zone data and a `nonroot` user:

    $ make demo-arch
    image:   bin/static-amd64.tar (OCI layout, linux/amd64, 1 layer)
    program: /app (--binary bin/hello-arm64), Go program, CGO_ENABLED=0
      ok    user            runs as nonroot (uid 65532, gid 65532)
      ok    permission      /app is executable (mode 0755)
      ERROR architecture    /app is arm64 but the image is amd64
                            exec fails with: exec format error
                            build with GOARCH=amd64, or build the image for arm64
                            the loader and library checks were skipped, because the kernel stops here
      ok    ca-certificates CA certificates found at /etc/ssl/certs/ca-certificates.crt
      ok    time-zones      time zone data found in /usr/share/zoneinfo
      1 error, 0 warnings: this program won't start in this image as it is.

The fixed build, a plain amd64 build, in the same image. It must stay quiet and exit 0:

    $ make demo-fixed
    image:   bin/static-amd64.tar (OCI layout, linux/amd64, 1 layer)
    program: /app (--binary bin/hello), Go program, CGO_ENABLED=0
      ok    user            runs as nonroot (uid 65532, gid 65532)
      ok    permission      /app is executable (mode 0755)
      ok    architecture    /app is amd64, the same as the image
      ok    loader          /app is statically linked and needs no loader or shared libraries
      ok    ca-certificates CA certificates found at /etc/ssl/certs/ca-certificates.crt
      ok    time-zones      time zone data found in /usr/share/zoneinfo
      No problems found in the checks above.

## Checks

| Check | Finds | What the container shows |
|---|---|---|
| file | The program isn't in the image, or isn't on `PATH` | `exec: "app": executable file not found in $PATH` |
| permission | No exec bit for the image user | `exec /app: permission denied` |
| user | The image user isn't in `/etc/passwd`, or a group isn't in `/etc/group` | `unable to find user app: no matching entries in passwd file` |
| format, architecture | A program for another OS or CPU, or a loader for another CPU | `exec format error` |
| loader | The ELF loader (`PT_INTERP`) is missing, including glibc programs in musl (Alpine) images | `exec /app: no such file or directory` |
| library | A shared library isn't on the loader's search path (RPATH, RUNPATH, `LD_LIBRARY_PATH`, `ld.so.cache`, default and multiarch directories, musl's path file) | `error while loading shared libraries: libfoo.so.1` |
| symbol-version | A glibc symbol version the program or its libraries need is missing | ``version `GLIBC_2.38' not found`` |
| script | The script's interpreter is missing, or the `#!` line ends in CRLF | `exec /app: no such file or directory` |
| ca-certificates | A Go program loads system roots and the image has none (WARN) | `x509: certificate signed by unknown authority` |
| time-zones | A Go program calls `time.LoadLocation` or the image sets `TZ`, and the image has no zone data (WARN) | `unknown time zone Europe/Berlin` |

Layers, whiteouts, opaque directories, hard links and symlinks are applied the way a container runtime does, with `..` stopping at the image root. It reads OCI layouts (tar or directory), `docker save` archives, and root filesystem tars (plain or gzip) or directories. Multi-platform images need `--platform` when there's more than one match.

## Limits

- **It reports problems it found in the checks it ran. It doesn't prove a program will start.** Anything at run time is out of scope: config files, mounts, network, secrets, the kernel, seccomp or the container runtime's settings.
- **Scripts:** it checks the interpreter of an entrypoint script, then says the program the script starts wasn't checked. Run `--exec` on that program too.
- **`dlopen`:** libraries loaded by name at run time, such as glibc's NSS modules or plugins, aren't checked.
- **Symbols:** it compares symbol versions, not individual symbols, so a symbol missing from a library whose version is present isn't found.
- **Layers compressed with zstd aren't read yet.** The tool exits 2 for them rather than guessing.
- It doesn't pull images from a registry. Get the archive with your own tools first.

## How it compares

- [container-structure-test](https://github.com/GoogleContainerTools/container-structure-test) runs commands and file checks you write, and needs a container runtime for command tests. `wontexec` needs no runtime and no test file, and knows the loader, library and Go-specific failures.
- [dockle](https://github.com/goodwithtech/dockle) checks images against security best practices, and [hadolint](https://github.com/hadolint/hadolint) lints Dockerfiles. Neither checks whether the program will start.
- `ldd` and `lddtree` read libraries from the machine they run on. `wontexec` reads them from the image, for the image's architecture.

## FAQ

**Why does my container say `exec /app: no such file or directory` when `/app` is in the image?** The kernel reports the missing ELF loader with the program's name. A cgo build, or a `-buildmode=pie` build even with `CGO_ENABLED=0`, needs `/lib64/ld-linux-x86-64.so.2` (or the arm64 or musl loader), which scratch, distroless/static and Alpine don't have. The `loader` check finds it and names the build flag.

**What does `exec format error` mean in Kubernetes?** The program was built for another CPU, usually an arm64 build from a Mac in an amd64 cluster or the reverse. The `architecture` check compares the program with the image's platform.

**How do I fix `version GLIBC_2.34 not found`?** The program was built against a newer glibc than the image has. Build in an image with the same or older glibc, use a newer base image, or build with `CGO_ENABLED=0`. The output says which version is missing and the newest one the image has.

**Does a Go program in scratch need CA certificates?** Yes, if it makes TLS connections with the system roots. `wontexec` looks for `crypto/x509.loadSystemRoots` in the program's function table, which stays even in stripped builds, and warns when the image has no certificates.

**Does it need Docker?** No. It reads files only. You need Docker, Podman, skopeo, crane or BuildKit to get the image as a file in the first place.

**Is it an MCP server?** No. It's a skill with one Go source file, so `go run` is all it needs.

## Tested on

- **Integration tests** (`tests/`) build real Go programs for linux/amd64, linux/arm64 and other targets with explicit `GOOS` and `GOARCH`, write OCI layouts, `docker save` archives and root filesystems around them, and check every finding and the healthy cases that must stay quiet. The library and glibc version tests build a cgo program on the host and use the host's real loader and C library, and skip without a C compiler.
- **Damaged input tests** feed empty files, garbage, archives cut at many points (including exact tar block and member boundaries), flipped bytes in a layer, a missing blob, a broken config, partly copied OCI layouts, the saved output of a real failed `go build`, and the error text a failed `docker save` prints. Each is used as the image and as the program where it applies. Each must exit 2 and print no clean result, with and without `--fail-on`.
- **Real images**, pulled as OCI layouts on 2026-11-06: `gcr.io/distroless/static-debian12`, `gcr.io/distroless/base-debian12`, `alpine:3.20` and `debian:bookworm-slim` for amd64, and `debian:bookworm-slim` for arm64. A static Go build passes on both distroless images, warns about time zone data on Alpine and about CA certificates on debian-slim. A PIE build fails on distroless/static and Alpine and passes on distroless/base. The images' own `/bin/sh`, `bash` and `apt-get` (18 shared libraries) stay quiet. A cgo program built against glibc 2.41 that calls `strlcpy` is reported as needing `GLIBC_2.38` in arm64 bookworm, which has 2.36.
- CI runs on Linux amd64 with Go 1.22 and the latest Go. Development was on Linux arm64.
- **Not tested:** real `docker save` and `podman save` archives (the tests write that format themselves), images built with zstd layers, Windows and macOS hosts, and the skill in a fresh Claude session. The findings weren't confirmed by starting a real container, since the test machine has no container runtime.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/wontexec/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `wontexec` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
