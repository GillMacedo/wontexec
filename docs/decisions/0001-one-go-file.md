# 0001: One standard-library Go file

**Decision:** the tool is a single Go file, `plugin/scripts/wontexec.go`, using only the standard library.

**Why:** the people who hit these failures build Go services and already have Go, so `go run` is the whole install. `archive/tar`, `compress/gzip`, `debug/elf` and `debug/buildinfo` cover every format it reads. A single file runs with `go run path/to/wontexec.go` from the plugin folder, where there is no go.mod.

**Alternatives:** `go-containerregistry` would add registry pulls and more layer formats, but brings a large dependency tree and network access the tool shouldn't need. An MCP server would give an agent the same command with more setup and nothing a script can't do.
