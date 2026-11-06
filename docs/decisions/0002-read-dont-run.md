# 0002: Read the image, don't run it

**Decision:** wontexec reads the image archive and the program as data. It doesn't start containers, extract to disk, or run the program, even through the image's loader.

**Why:** the question comes up in CI and on laptops without Docker or root, and in reviews of an image someone else built. Running untrusted programs to check them is a risk the user didn't ask for. Reading is also deterministic across machines: a check of a linux/amd64 image gives the same answer on an arm64 Mac.

**Cost:** a static read can't see what happens later: programs started by scripts, libraries loaded with `dlopen`, files mounted at run time, or symbols missing from a library whose version is present. The README lists these limits, and the output marks what it skipped.
