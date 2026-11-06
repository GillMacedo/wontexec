# Examples

- `hello/` is a made-up service. It calls an HTTPS URL given as an argument and prints the time in a named zone, so it needs CA certificates and time zone data at run time.
- `mkimage/` writes a small OCI image layout tar for the demos. The certificate and zone files it adds are placeholders, enough for wontexec's checks but not for real use.

`make demo-files` builds `hello` three ways, all with `CGO_ENABLED=0`: a `-buildmode=pie` build for linux/amd64, a plain build for linux/arm64, and a plain build for linux/amd64. It also writes two images: `bin/scratch-amd64.tar`, an empty linux/amd64 image, and `bin/static-amd64.tar`, a linux/amd64 image that runs as `nonroot` and has certificates, zone data and `/etc/passwd`.

- `make demo-pie` checks the PIE build in the empty image: the loader is missing.
- `make demo-arch` checks the arm64 build in the amd64 image.
- `make demo-fixed` checks the plain amd64 build in the second image, which must pass.
