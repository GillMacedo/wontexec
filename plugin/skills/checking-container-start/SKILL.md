---
name: checking-container-start
description: Checks, without running anything, whether a Linux program will start inside a container image, from a docker save tar, an OCI layout or a root filesystem. Finds a missing ELF loader ("exec /app no such file or directory"), a wrong CPU architecture ("exec format error"), shared libraries the loader can't find, "version GLIBC_2.34 not found", an image user missing from /etc/passwd, a program without the exec bit, CRLF script lines, and for Go programs missing CA certificates or time zone data. Use when a pod is in CrashLoopBackOff right after a base image or build change, when moving a Go service to scratch, distroless or Alpine, or before pushing a new image.
---

# Checking that a program will start in a container image

The bundled tool needs Go. It reads the image archive and the program as data and never runs them.

## 1. Get the image as a file

Ask before pulling or saving images. Use what the project already has:

- `docker save IMAGE -o image.tar` or `podman save IMAGE -o image.tar`
- `skopeo copy docker://IMAGE oci-archive:image.tar`, or `crane pull IMAGE image.tar`
- a root filesystem: `docker export CONTAINER > rootfs.tar`, or BuildKit `--output type=local,dest=DIR`

For a multi-platform image, pass `--platform linux/amd64` (or the platform the cluster runs).

## 2. Run the check

The image's own entrypoint:

    go run "${CLAUDE_PLUGIN_ROOT}/scripts/wontexec.go" image.tar

A program built on this machine, as if copied into a base image (the usual case before a Dockerfile change):

    go run "${CLAUDE_PLUGIN_ROOT}/scripts/wontexec.go" --binary ./bin/app --at /app gcr-base.tar

Other flags:
- `--exec /usr/local/bin/app` checks another program in the image, such as the one a Kubernetes `command:` runs, or the program an entrypoint script starts.
- `--user 65532` checks as the pod's `runAsUser`.
- `--format json` for parsing; `--fail-on error` to ignore warnings in the exit code.

When the entrypoint is a script, the tool checks its interpreter and says the program the script starts wasn't checked. Read the script and run `--exec` on that program too.

## Exit codes

0 means no findings. 1 means findings. 2 means bad usage or an input it couldn't read fully (an empty, cut-off or damaged archive or program). Treat 2 as unknown, never as a pass.

## Report

1. For each ERROR, give the error the container would show and the smallest fix in the project's Dockerfile or build flags (for Go: `CGO_ENABLED=0`, dropping `-buildmode=pie`, `GOARCH`, a base image with the same C library).
2. For each WARN, say when it bites (TLS calls, named time zones) and the fix.
3. Say what was checked (platform, which program) and what wasn't: the program behind a script, plugins loaded with dlopen, and anything at run time such as config files, mounts or network.
4. Don't edit Dockerfiles or build files unless the user asks. Don't run the image or its programs.
