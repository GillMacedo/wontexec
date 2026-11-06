# Architecture

    loadImage:  OCI index.json --> manifest --> config + layers   (digests checked)
                docker save manifest.json --> config + layers      (diff_ids checked)
                root filesystem tar(.gz) or directory
                each layer --> walkTar (end block required) --> imageFS (whiteouts, opaque dirs, hard links)
    run:        entrypoint or --exec or --binary --> checkUser --> checkProgram
    checkFile:  script? --> interpreter --> checkFile again (once)
                ELF --> readGo --> checkArch --> checkLoader --> checkLibs --> checkVersions --> checkGo
    output:     text or JSON --> exit code from --fail-on

- **The image is a tree in memory.** Small files and anything named like a shared library are kept as bytes. Larger files are re-read from their layer when needed, which is only the program itself in practice. Symlinks resolve inside the image, with `..` stopping at the root, the way the kernel does for a container's root.
- **Every read is checked.** Blobs must match their digests, uncompressed layers must match the config's `diff_ids`, and every tar stream must end with an end-of-archive block, because `archive/tar` returns a clean end of file when a stream is cut exactly between members. ELF segments and sections must lie inside the file. Anything else exits 2.
- **Library search follows the loader.** For glibc: `DT_RPATH` (when there is no `DT_RUNPATH`), `LD_LIBRARY_PATH` from the image config, `DT_RUNPATH`, `/etc/ld.so.cache`, then the default directories including Debian's multiarch ones. For musl: `LD_LIBRARY_PATH`, the object's paths, then `/etc/ld-musl-ARCH.path` or `/lib:/usr/local/lib:/usr/lib`. A file for another architecture is skipped, as the loader skips it.
- **Symbol versions come from the ELF tables.** `.gnu.version_r` of each object lists the versions it needs from each library; `.gnu.version_d` of the library lists what it defines. Weak requirements are ignored.
- **Go checks read the function name table.** `.gopclntab` holds every linked function's name, even in stripped builds, so the tool looks for `crypto/x509.loadSystemRoots`, `time.LoadLocation`, `time/tzdata.` and the `x509roots/fallback` package by name. Build info gives `CGO_ENABLED` and `-buildmode` for the hints.

Key choices are in `decisions/`.
