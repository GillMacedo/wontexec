# Roadmap

## Next
- Read zstd-compressed layers, which some registries and BuildKit can produce
- Check symbols, not only symbol versions, against the libraries in the image
- A GitHub Action example that runs wontexec on the image a workflow just built, for every platform

## Later
- Follow the program an entrypoint script starts, for simple `exec /path` lines
- Check `dlopen` targets that glibc loads by name, such as the NSS modules a cgo DNS lookup uses
- Read a Kubernetes pod spec for `command`, `runAsUser` and env, instead of passing them as flags
