// Package tests runs the wontexec command against real Go binaries built by
// the toolchain on the test machine, inside image archives written here.
// Every build picks its GOOS and GOARCH explicitly, so the results don't
// depend on the machine running the tests. The cgo tests use the host's own
// C library and skip when there is no C compiler.
package tests

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	toolPath  string
	buildErr  error
	binCache  sync.Map
	tmpRoot   string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wontexec-tests")
	if err != nil {
		panic(err)
	}
	tmpRoot = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Dir(wd)
}

func tool(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		toolPath = filepath.Join(tmpRoot, "wontexec")
		if runtime.GOOS == "windows" {
			toolPath += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", toolPath, "./plugin/scripts")
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = &buildError{string(out)}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return toolPath
}

type buildError struct{ out string }

func (b *buildError) Error() string { return "build failed: " + b.out }

// goBuild builds examples/hello for a target, once per set of arguments.
func goBuild(t *testing.T, goos, goarch, cgo string, flags ...string) string {
	t.Helper()
	key := strings.Join(append([]string{goos, goarch, cgo}, flags...), " ")
	if p, ok := binCache.Load(key); ok {
		return p.(string)
	}
	sum := sha256.Sum256([]byte(key))
	out := filepath.Join(tmpRoot, "hello-"+hex.EncodeToString(sum[:6]))
	args := append([]string{"build", "-o", out}, flags...)
	args = append(args, "./examples/hello")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED="+cgo, "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", key, err, b)
	}
	binCache.Store(key, out)
	return out
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type entry struct {
	name string
	body []byte
	mode int64
	typ  byte
	link string
	uid  int
	gid  int
}

func file(name string, body []byte, mode int64) entry {
	return entry{name: name, body: body, mode: mode, typ: tar.TypeReg}
}
func symlink(name, target string) entry {
	return entry{name: name, link: target, mode: 0o777, typ: tar.TypeSymlink}
}
func dir(name string) entry { return entry{name: name, mode: 0o755, typ: tar.TypeDir} }

func tarBytes(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link, Uid: e.uid, Gid: e.gid}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write(e.body)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

type cfg struct {
	arch, user string
	entry, cmd []string
	env        []string
	workdir    string
}

func configJSON(c cfg, diffIDs []string) []byte {
	b, _ := json.Marshal(map[string]any{
		"architecture": c.arch,
		"os":           "linux",
		"config":       map[string]any{"Entrypoint": c.entry, "Cmd": c.cmd, "User": c.user, "Env": c.env, "WorkingDir": c.workdir},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	return b
}

// writeOCI writes an OCI layout tar with gzip layers. It returns the path
// and the member names in order, for damage tests.
func writeOCI(t *testing.T, c cfg, layers ...[]entry) string {
	t.Helper()
	members := ociMembers(t, c, layers...)
	return writeOuter(t, members)
}

type member struct {
	name string
	body []byte
}

func ociMembers(t *testing.T, c cfg, layers ...[]entry) []member {
	t.Helper()
	var diffs []string
	var blobs [][]byte
	var descs []map[string]any
	for _, l := range layers {
		raw := tarBytes(t, l)
		diffs = append(diffs, digest(raw))
		z := gz(raw)
		blobs = append(blobs, z)
		descs = append(descs, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": digest(z), "size": len(z)})
	}
	cb := configJSON(c, diffs)
	mb, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(cb), "size": len(cb)},
		"layers":        descs,
	})
	ib, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []map[string]any{{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest(mb), "size": len(mb)}}})
	ms := []member{{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", ib}}
	for _, b := range append([][]byte{cb, mb}, blobs...) {
		ms = append(ms, member{"blobs/sha256/" + strings.TrimPrefix(digest(b), "sha256:"), b})
	}
	return ms
}

func writeOuter(t *testing.T, ms []member) string {
	t.Helper()
	var es []entry
	for _, m := range ms {
		es = append(es, file(m.name, m.body, 0o644))
	}
	p := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(p, tarBytes(t, es), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeDockerSave writes the older docker save format, with uncompressed layers.
func writeDockerSave(t *testing.T, c cfg, layers ...[]entry) string {
	t.Helper()
	var diffs, names []string
	var ms []member
	for i, l := range layers {
		raw := tarBytes(t, l)
		diffs = append(diffs, digest(raw))
		name := strings.Repeat(string(rune('a'+i)), 8) + "/layer.tar"
		names = append(names, name)
		ms = append(ms, member{name, raw})
	}
	cb := configJSON(c, diffs)
	mj, _ := json.Marshal([]map[string]any{{"Config": "config.json", "RepoTags": []string{"demo:latest"}, "Layers": names}})
	ms = append(ms, member{"config.json", cb}, member{"manifest.json", mj})
	return writeOuter(t, ms)
}

type result struct {
	out, errOut string
	code        int
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(tool(t), args...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{o.String(), e.String(), code}
}

func expect(t *testing.T, r result, code int, wants ...string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.out, r.errOut)
	}
	all := r.out + r.errOut
	for _, w := range wants {
		if !strings.Contains(all, w) {
			t.Errorf("output lacks %q\nstdout:\n%s\nstderr:\n%s", w, r.out, r.errOut)
		}
	}
}

// expectUntrusted checks an input that must never pass, with or without --fail-on none.
func expectUntrusted(t *testing.T, args ...string) {
	t.Helper()
	for _, extra := range [][]string{nil, {"--fail-on", "none"}, {"--fail-on", "error"}, {"--format", "json"}} {
		r := run(t, append(append([]string{}, extra...), args...)...)
		if r.code != 2 {
			t.Errorf("%v: exit %d, want 2\nstdout:\n%s\nstderr:\n%s", append(extra, args...), r.code, r.out, r.errOut)
		}
		if strings.Contains(r.out, "No problems found") || strings.Contains(r.out, `"errors": 0`) {
			t.Errorf("%v printed a clean result:\n%s", args, r.out)
		}
		if !strings.Contains(r.errOut, "This is not a pass") {
			t.Errorf("%v: stderr doesn't say the result is unknown:\n%s", args, r.errOut)
		}
	}
}

var (
	certs  = file("etc/ssl/certs/ca-certificates.crt", []byte("-----BEGIN CERTIFICATE-----\nplaceholder\n-----END CERTIFICATE-----\n"), 0o644)
	tzdata = file("usr/share/zoneinfo/UTC", []byte("TZif placeholder"), 0o644)
	passwd = file("etc/passwd", []byte("root:x:0:0::/root:/sbin/nologin\nnonroot:x:65532:65532::/home/nonroot:/sbin/nologin\n"), 0o644)
	group  = file("etc/group", []byte("root:x:0:\nnonroot:x:65532:\n"), 0o644)
)

func amd64Static(t *testing.T) string { return goBuild(t, "linux", "amd64", "0") }

// ---------- healthy cases ----------

func TestHealthyStaticStaysQuiet(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, user: "nonroot"},
		[]entry{certs, tzdata, passwd, group}, []entry{file("app", bin, 0o755)})
	r := run(t, img)
	expect(t, r, 0, "No problems found", "statically linked", "CA certificates found", "time zone data found", "runs as nonroot (uid 65532")
	if strings.Contains(r.out, "WARN") || strings.Contains(r.out, "ERROR") {
		t.Errorf("healthy image printed findings:\n%s", r.out)
	}
	// The same image as docker save and as a plain root filesystem tar.gz.
	r = run(t, writeDockerSave(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata, file("app", bin, 0o755)}))
	expect(t, r, 0, "docker save", "No problems found")
	rootfs := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	os.WriteFile(rootfs, gz(tarBytes(t, []entry{certs, tzdata, file("app", bin, 0o755)})), 0o644)
	expect(t, run(t, "--exec", "/app", "--platform", "linux/amd64", rootfs), 0, "root filesystem tar", "No problems found")
	// Without --platform the architecture is reported as skipped, not as a pass.
	expect(t, run(t, "--exec", "/app", rootfs), 0, "skip  architecture", "1 check skipped")
}

func TestHealthyRootDirectory(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	root := t.TempDir()
	for _, e := range []entry{certs, tzdata, file("app", bin, 0o755)} {
		p := filepath.Join(root, filepath.FromSlash(e.name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, e.body, os.FileMode(e.mode))
	}
	expect(t, run(t, "--exec", "/app", "--platform", "linux/amd64", root), 0, "root filesystem directory", "No problems found")
}

func TestEmbeddedRootsAndZones(t *testing.T) {
	// -tags timetzdata embeds zones; an image with neither certs nor zones then
	// warns only about certificates.
	bin := readFile(t, goBuild(t, "linux", "amd64", "0", "-tags", "timetzdata"))
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("app", bin, 0o755)})
	r := run(t, img)
	expect(t, r, 1, "embeds time zone data", "WARN  ca-certificates")
	expect(t, run(t, "--fail-on", "error", img), 0, "0 errors, 1 warning")
}

// ---------- findings ----------

func TestScratchWarnsCertsAndZones(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("app", bin, 0o755)})
	expect(t, run(t, img), 1, "WARN  ca-certificates", "certificate signed by unknown authority", "WARN  time-zones", "unknown time zone")
	expect(t, run(t, "--fail-on", "none", img), 0, "0 errors, 2 warnings")
	// SSL_CERT_FILE pointing nowhere is a warning even when the default file exists.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, env: []string{"SSL_CERT_FILE=/certs/missing.pem"}}, []entry{certs, tzdata, file("app", bin, 0o755)})
	expect(t, run(t, img), 1, "SSL_CERT_FILE=/certs/missing.pem is set but holds no certificates")
	// TZ naming a zone the image doesn't have.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, env: []string{"TZ=Asia/Tokyo"}}, []entry{certs, tzdata, file("app", bin, 0o755)})
	expect(t, run(t, img), 1, "sets TZ=Asia/Tokyo but has no zone file")
}

func TestPIENeedsLoader(t *testing.T) {
	bin := readFile(t, goBuild(t, "linux", "amd64", "0", "-buildmode=pie"))
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata, file("app", bin, 0o755)})
	expect(t, run(t, img), 1, "ERROR loader", "/lib64/ld-linux-x86-64.so.2, which is not in the image", "-buildmode=pie build")
	expect(t, run(t, "--fail-on", "none", img), 0, "1 error")
}

func TestArchitectureMismatch(t *testing.T) {
	for _, c := range []struct{ binArch, imgArch string }{{"arm64", "amd64"}, {"amd64", "arm64"}, {"riscv64", "amd64"}} {
		bin := readFile(t, goBuild(t, "linux", c.binArch, "0"))
		img := writeOCI(t, cfg{arch: c.imgArch, entry: []string{"/app"}}, []entry{certs, tzdata, file("app", bin, 0o755)})
		expect(t, run(t, img), 1, "ERROR architecture", "/app is "+c.binArch+" but the image is "+c.imgArch, "exec format error")
	}
	// The image architecture can also be given with --platform for a root filesystem.
	bin := readFile(t, goBuild(t, "linux", "arm64", "0"))
	rootfs := filepath.Join(t.TempDir(), "rootfs.tar")
	os.WriteFile(rootfs, tarBytes(t, []entry{file("app", bin, 0o755)}), 0o644)
	expect(t, run(t, "--exec", "/app", "--platform", "linux/amd64", rootfs), 1, "ERROR architecture")
}

func TestOtherOperatingSystems(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		bin := readFile(t, goBuild(t, goos, "amd64", "0"))
		img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("app", bin, 0o755)})
		expect(t, run(t, img), 1, "ERROR format", "Linux can't run")
	}
}

func TestFilePermissionAndUser(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	base := []entry{certs, tzdata, passwd, group}
	// Owned by root, mode 0700, run as nonroot.
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, user: "nonroot"}, base, []entry{file("app", bin, 0o700)})
	expect(t, run(t, img), 1, "ERROR permission", "not executable for the image user (mode 0700")
	// The same file is fine for root.
	expect(t, run(t, "--user", "0", img), 0, "No problems found")
	// No exec bit at all.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, base, []entry{file("app", bin, 0o644)})
	expect(t, run(t, img), 1, "ERROR permission", "permission denied")
	// A user name the image doesn't have.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, user: "appuser"}, base, []entry{file("app", bin, 0o755)})
	expect(t, run(t, img), 1, `ERROR user`, `user "appuser" can't be found`)
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, user: "nonroot"}, []entry{certs, tzdata, file("app", bin, 0o755)})
	expect(t, run(t, img), 1, "the image has no /etc/passwd")
	// Numeric users need no passwd entry.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, user: "65532:65532"}, []entry{certs, tzdata, file("app", bin, 0o755)})
	expect(t, run(t, img), 0, "runs as 65532:65532")
	// Group bits apply to a member of the file's group.
	owned := file("app", bin, 0o750)
	owned.uid, owned.gid = 0, 65532
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}, user: "nonroot"}, base, []entry{owned})
	expect(t, run(t, img), 0, "No problems found")
}

func TestMissingProgramAndPaths(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	base := []entry{certs, tzdata}
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, base, []entry{dir("srv"), file("srv/app", bin, 0o755)})
	expect(t, run(t, img), 1, "ERROR file", "/app is not in the image", "a file with that name is at /srv/app")
	// A bare name is looked up on the image's PATH.
	img = writeOCI(t, cfg{arch: "amd64", cmd: []string{"app"}, env: []string{"PATH=/usr/bin"}}, base, []entry{file("srv/app", bin, 0o755)})
	expect(t, run(t, img), 1, `"app" is not in any PATH directory`)
	img = writeOCI(t, cfg{arch: "amd64", cmd: []string{"app"}, env: []string{"PATH=/usr/bin:/srv"}}, base, []entry{file("srv/app", bin, 0o755)})
	expect(t, run(t, img), 0, "No problems found")
	// Relative paths use WorkingDir, and symlinks resolve inside the image.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"./run"}, workdir: "/srv"}, base,
		[]entry{file("opt/app/bin/app", bin, 0o755), symlink("srv/run", "../opt/app/bin/app")})
	expect(t, run(t, img), 0, "/srv/run -> /opt/app/bin/app")
	// A symlink loop.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/a"}}, base, []entry{symlink("a", "b"), symlink("b", "a")})
	expect(t, run(t, img), 1, "too many levels of symbolic links")
	// The entrypoint is a directory.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/srv"}}, base, []entry{dir("srv")})
	expect(t, run(t, img), 1, "is a directory")
}

func TestWhiteouts(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	lower := []entry{certs, tzdata, file("app", bin, 0o755), file("opt/tool", bin, 0o755)}
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, lower, []entry{file(".wh.app", nil, 0o644)})
	expect(t, run(t, img), 1, "/app is not in the image")
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/opt/tool"}}, lower, []entry{dir("opt"), file("opt/.wh..wh..opq", nil, 0o644)})
	expect(t, run(t, img), 1, "/opt/tool is not in the image")
	// Removing the certificates in an upper layer is seen.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, lower, []entry{file("etc/ssl/certs/.wh.ca-certificates.crt", nil, 0o644)})
	expect(t, run(t, img), 1, "WARN  ca-certificates")
	// A file replaced in an upper layer uses the upper copy.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata, file("app", []byte("#!/bin/missing\n"), 0o755)}, []entry{file("app", bin, 0o755)})
	expect(t, run(t, img), 0, "No problems found")
}

func TestScripts(t *testing.T) {
	sh := readFile(t, amd64Static(t)) // stands in for /bin/sh: a real amd64 ELF
	base := []entry{certs, tzdata, file("bin/sh", sh, 0o755)}
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/entrypoint.sh"}}, base, []entry{file("entrypoint.sh", []byte("#!/bin/sh\nexec /app\n"), 0o755)})
	expect(t, run(t, img), 0, "is a script run by /bin/sh", "skip  script", "1 check skipped")
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/entrypoint.sh"}}, base, []entry{file("entrypoint.sh", []byte("#!/bin/sh\r\nexec /app\r\n"), 0o755)})
	expect(t, run(t, img), 1, "Windows line endings", "dos2unix")
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/entrypoint.sh"}}, base, []entry{file("entrypoint.sh", []byte("#!/bin/bash\nexec /app\n"), 0o755)})
	expect(t, run(t, img), 1, "the script interpreter /bin/bash is not in the image")
	// The interpreter is for another architecture.
	other := readFile(t, goBuild(t, "linux", "arm64", "0"))
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/entrypoint.sh"}}, []entry{file("bin/sh", other, 0o755)}, []entry{file("entrypoint.sh", []byte("#!/bin/sh\n"), 0o755)})
	expect(t, run(t, img), 1, "ERROR architecture", "/bin/sh is arm64")
	// sh -c commands are reported as not checked.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/bin/sh", "-c", "exec /app"}}, base)
	expect(t, run(t, img), 0, "runs a shell command (-c)")
}

func TestMultiPlatformIndex(t *testing.T) {
	amd := readFile(t, amd64Static(t))
	arm := readFile(t, goBuild(t, "linux", "arm64", "0"))
	a := ociMembers(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata, file("app", amd, 0o755)})
	b := ociMembers(t, cfg{arch: "arm64", entry: []string{"/app"}}, []entry{certs, tzdata, file("app", arm, 0o755)})
	// Merge the blobs and write one index with both manifests.
	var ms []member
	var manifests []map[string]any
	for _, set := range [][]member{a, b} {
		var idx struct{ Manifests []map[string]any }
		for _, m := range set {
			if m.name == "index.json" {
				json.Unmarshal(m.body, &idx)
			} else if strings.HasPrefix(m.name, "blobs/") {
				ms = append(ms, m)
			}
		}
		manifests = append(manifests, idx.Manifests[0])
	}
	manifests[0]["platform"] = map[string]string{"os": "linux", "architecture": "amd64"}
	manifests[1]["platform"] = map[string]string{"os": "linux", "architecture": "arm64", "variant": "v8"}
	ib, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": manifests})
	img := writeOuter(t, append([]member{{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", ib}}, ms...))
	expectUntrusted(t, img)
	expect(t, run(t, img), 2, "several platforms (linux/amd64, linux/arm64/v8)")
	expect(t, run(t, "--platform", "linux/arm64", img), 0, "linux/arm64,", "No problems found")
	expect(t, run(t, "--platform", "linux/amd64", img), 0, "linux/amd64", "No problems found")
	expect(t, run(t, "--platform", "linux/s390x", img), 2, "has no linux/s390x platform")
}

func TestJSON(t *testing.T) {
	bin := readFile(t, goBuild(t, "linux", "amd64", "0", "-buildmode=pie"))
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("app", bin, 0o755)})
	r := run(t, "--format", "json", img)
	var rep struct {
		Errors, Warnings int
		Go               struct {
			CGO string `json:"cgo_enabled"`
		}
		Findings []struct{ Check, Level string }
	}
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, r.out)
	}
	if r.code != 1 || rep.Errors != 1 || rep.Warnings != 2 || rep.Go.CGO != "0" {
		t.Errorf("unexpected report (exit %d): %+v", r.code, rep)
	}
}

// ---------- the musl loader ----------

// patchInterp rewrites the PT_INTERP path of a real binary, so a Go PIE build
// can stand in for a program linked against another C library.
func patchInterp(t *testing.T, b []byte, interp string) []byte {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			if uint64(len(interp)) >= p.Filesz {
				t.Fatalf("interp %q too long", interp)
			}
			out := append([]byte{}, b...)
			region := out[p.Off : p.Off+p.Filesz]
			for i := range region {
				region[i] = 0
			}
			copy(region, interp)
			return out
		}
	}
	t.Fatal("no PT_INTERP")
	return nil
}

func TestMuslAndGlibcLoaders(t *testing.T) {
	pie := readFile(t, goBuild(t, "linux", "amd64", "0", "-buildmode=pie"))
	static := readFile(t, amd64Static(t))
	musl := []entry{certs, tzdata, file("lib/ld-musl-x86_64.so.1", static, 0o755)}
	// A glibc program in a musl image.
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, musl, []entry{file("app", pie, 0o755)})
	expect(t, run(t, img), 1, "ERROR loader", "the image uses musl (Alpine) and the program was linked against glibc")
	// A musl program in a musl image.
	mprog := patchInterp(t, pie, "/lib/ld-musl-x86_64.so.1")
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, musl, []entry{file("app", mprog, 0o755)})
	expect(t, run(t, img), 0, "loader /lib/ld-musl-x86_64.so.1 is in the image (musl)", "No problems found")
	// A loader for the wrong architecture.
	arm := readFile(t, goBuild(t, "linux", "arm64", "0"))
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("lib/ld-musl-x86_64.so.1", arm, 0o755)}, []entry{file("app", mprog, 0o755)})
	expect(t, run(t, img), 1, "the loader /lib/ld-musl-x86_64.so.1 is arm64")
	// A loader that is cut off can't be trusted.
	img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("lib/ld-musl-x86_64.so.1", static[:len(static)/2], 0o755)}, []entry{file("app", mprog, 0o755)})
	expectUntrusted(t, img)
}

// ---------- shared libraries, with the host's real C library ----------

type hostLibs struct {
	interp string
	files  map[string][]byte // image path -> content
	prog   []byte
	arch   string
}

// hostCgo builds a cgo program natively and collects the host's loader and
// libraries for it. It skips when cgo isn't available.
func hostCgo(t *testing.T) *hostLibs {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("cgo library tests need a Linux host")
	}
	cc, _ := exec.Command("go", "env", "CC").Output()
	if _, err := exec.LookPath(strings.TrimSpace(string(cc))); err != nil {
		t.Skip("no C compiler")
	}
	prog := readFile(t, goBuild(t, "linux", runtime.GOARCH, "1"))
	f, err := elf.NewFile(bytes.NewReader(prog))
	if err != nil {
		t.Fatal(err)
	}
	h := &hostLibs{files: map[string][]byte{}, prog: prog, arch: runtime.GOARCH}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			b := make([]byte, p.Filesz)
			p.ReadAt(b, 0)
			h.interp = string(bytes.TrimRight(b, "\x00"))
		}
	}
	if h.interp == "" {
		t.Skip("the cgo build is static on this host")
	}
	real, err := filepath.EvalSymlinks(h.interp)
	if err != nil {
		t.Skip("host loader not found")
	}
	h.files[h.interp] = readFile(t, real)
	needed, _ := f.ImportedLibraries()
	for _, name := range needed {
		found := false
		for _, d := range []string{"/lib/" + triplet(), "/usr/lib/" + triplet(), "/lib64", "/usr/lib64", "/lib", "/usr/lib"} {
			p := path.Join(d, name)
			if r, err := filepath.EvalSymlinks(p); err == nil {
				h.files[p] = readFile(t, r)
				found = true
				break
			}
		}
		if !found {
			t.Skipf("host library %s not found", name)
		}
	}
	return h
}

func triplet() string {
	return map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu", "386": "i386-linux-gnu", "ppc64le": "powerpc64le-linux-gnu", "s390x": "s390x-linux-gnu", "riscv64": "riscv64-linux-gnu"}[runtime.GOARCH]
}

func (h *hostLibs) entries(skip string) []entry {
	var es []entry
	for p, b := range h.files {
		if p == skip {
			continue
		}
		es = append(es, file(strings.TrimPrefix(p, "/"), b, 0o755))
	}
	return es
}

func TestHostGlibcHealthy(t *testing.T) {
	h := hostCgo(t)
	img := writeOCI(t, cfg{arch: h.arch, entry: []string{"/app"}}, append(h.entries(""), certs, tzdata), []entry{file("app", h.prog, 0o755)})
	expect(t, run(t, img), 0, "loader "+h.interp+" is in the image (glibc)", "shared librar", "No problems found")
}

func TestHostGlibcMissingLibrary(t *testing.T) {
	h := hostCgo(t)
	var libc string
	for p := range h.files {
		if strings.HasSuffix(p, "/libc.so.6") {
			libc = p
		}
	}
	if libc == "" {
		t.Skip("program doesn't link libc.so.6 by that name")
	}
	img := writeOCI(t, cfg{arch: h.arch, entry: []string{"/app"}}, append(h.entries(libc), certs, tzdata), []entry{file("app", h.prog, 0o755)})
	expect(t, run(t, img), 1, "ERROR library", "needs libc.so.6, which the loader can't find", "cannot open shared object file")
	// The library is in the image, but off the search path.
	off := append(h.entries(libc), certs, tzdata, file("opt/lib/libc.so.6", h.files[libc], 0o755))
	img = writeOCI(t, cfg{arch: h.arch, entry: []string{"/app"}}, off, []entry{file("app", h.prog, 0o755)})
	expect(t, run(t, img), 1, "a file with that name is at /opt/lib/libc.so.6, which isn't on the search path")
	// LD_LIBRARY_PATH puts it back on the path.
	img = writeOCI(t, cfg{arch: h.arch, entry: []string{"/app"}, env: []string{"LD_LIBRARY_PATH=/opt/lib"}}, off, []entry{file("app", h.prog, 0o755)})
	expect(t, run(t, img), 0, "No problems found")
	// A copy for another architecture is skipped, the way the loader skips it.
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	wrong := readFile(t, goBuild(t, "linux", other, "0"))
	img = writeOCI(t, cfg{arch: h.arch, entry: []string{"/app"}}, append(h.entries(libc), certs, tzdata, file(strings.TrimPrefix(libc, "/"), wrong, 0o755)), []entry{file("app", h.prog, 0o755)})
	expect(t, run(t, img), 1, "found only for another architecture")
}

// TestHostGlibcTooOld builds a stand-in libc.so.6 with the real C compiler and
// a version script that defines only GLIBC_2.0, so every version the program
// needs is missing.
func TestHostGlibcTooOld(t *testing.T) {
	h := hostCgo(t)
	var libc string
	for p := range h.files {
		if strings.HasSuffix(p, "/libc.so.6") {
			libc = p
		}
	}
	if libc == "" {
		t.Skip("program doesn't link libc.so.6 by that name")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "old.c"), []byte("int old_symbol(void) { return 1; }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "old.map"), []byte("GLIBC_2.0 { global: old_symbol; local: *; };\n"), 0o644)
	cc, _ := exec.Command("go", "env", "CC").Output()
	cmd := exec.Command(strings.TrimSpace(string(cc)), "-shared", "-fPIC", "-nostdlib", "-Wl,-soname,libc.so.6", "-Wl,--version-script=old.map", "-o", "libc.so.6", "old.c")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("can't build the stand-in library: %v\n%s", err, out)
	}
	old := readFile(t, filepath.Join(dir, "libc.so.6"))
	img := writeOCI(t, cfg{arch: h.arch, entry: []string{"/app"}}, append(h.entries(libc), certs, tzdata, file(strings.TrimPrefix(libc, "/"), old, 0o755)), []entry{file("app", h.prog, 0o755)})
	expect(t, run(t, img), 1, "ERROR symbol-version", "version `GLIBC_2.", "not found (required by /app)", "the newest matching version in the image is GLIBC_2.0")
}

// ---------- inputs that can't be trusted ----------

func TestDamagedImages(t *testing.T) {
	bin := readFile(t, amd64Static(t))
	dirT := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dirT, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	failed := []byte("Error response from daemon: reference does not exist\n")
	expectUntrusted(t, write("empty.tar", nil))
	expectUntrusted(t, write("garbage.tar", bytes.Repeat([]byte("garbage!"), 700)))
	expectUntrusted(t, write("failed.tar", failed))
	expectUntrusted(t, write("failed.tar.gz", gz(failed)))
	expectUntrusted(t, filepath.Join(dirT, "does-not-exist.tar"))

	for _, format := range []string{"oci", "docker"} {
		var img string
		if format == "oci" {
			img = writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata}, []entry{file("app", bin, 0o755)})
		} else {
			img = writeDockerSave(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata}, []entry{file("app", bin, 0o755)})
		}
		expect(t, run(t, img), 0, "No problems found")
		whole := readFile(t, img)
		// Cut at many points, including exact 512-byte block boundaries.
		cuts := []int{1, 100, 511, 512, 1024, 1536, len(whole) / 3, len(whole) / 2, len(whole) - 2048, len(whole) - 1536, len(whole) - 1025}
		for _, c := range cuts {
			expectUntrusted(t, write(format+"-cut.tar", whole[:c]))
		}
		// Every member boundary of the outer archive.
		for _, c := range memberEnds(t, whole) {
			expectUntrusted(t, write(format+"-member-cut.tar", whole[:c]))
		}
		// A flipped byte in the middle of the program's layer.
		bad := append([]byte{}, whole...)
		i := bytes.Index(bad, bin[4096:4160])
		if format == "oci" {
			i = len(bad) / 2
		}
		if i < 0 {
			t.Fatal("layer bytes not found")
		}
		bad[i] ^= 0xff
		expectUntrusted(t, write(format+"-flipped.tar", bad))
	}

	// A root filesystem tar.gz cut short, and one cut at a member boundary.
	raw := tarBytes(t, []entry{certs, tzdata, file("app", bin, 0o755)})
	z := gz(raw)
	expectUntrusted(t, "--exec", "/app", write("cut.tar.gz", z[:len(z)/2]))
	expectUntrusted(t, "--exec", "/app", write("cut2.tar.gz", z[:len(z)-4]))
	for _, c := range memberEnds(t, raw) {
		expectUntrusted(t, "--exec", "/app", "--platform", "linux/amd64", write("boundary.tar", raw[:c]))
		expectUntrusted(t, "--exec", "/app", "--platform", "linux/amd64", write("boundary.tar.gz", gz(raw[:c])))
	}

	// An OCI layout whose manifest points at a missing blob.
	ms := ociMembers(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{file("app", bin, 0o755)})
	expectUntrusted(t, writeOuter(t, ms[:len(ms)-1]))
	// An OCI layout copied only in part: without index.json, without oci-layout,
	// or only its blobs. As a tar and as a directory, with --binary so that a
	// root filesystem reading would have nothing else to complain about.
	for _, drop := range []string{"index.json", "oci-layout", "both"} {
		var kept []member
		for _, m := range ms {
			if m.name == drop || (drop == "both" && (m.name == "index.json" || m.name == "oci-layout")) {
				continue
			}
			kept = append(kept, m)
		}
		expectUntrusted(t, "--binary", amd64Static(t), "--at", "/app", writeOuter(t, kept))
		dir := t.TempDir()
		for _, m := range kept {
			p := filepath.Join(dir, filepath.FromSlash(m.name))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, m.body, 0o644)
		}
		expectUntrusted(t, "--binary", amd64Static(t), "--at", "/app", dir)
	}
	// A config that is not JSON.
	for i, m := range ms {
		if strings.HasPrefix(m.name, "blobs/") && bytes.HasPrefix(m.body, []byte(`{"architecture"`)) {
			broken := append([]member{}, ms...)
			broken[i] = member{m.name, m.body[:len(m.body)/2]}
			expectUntrusted(t, writeOuter(t, broken))
		}
	}
	// A config without a program, and a root filesystem with no --exec.
	expect(t, run(t, writeOCI(t, cfg{arch: "amd64"}, []entry{certs})), 2, "no entrypoint or command")
	expect(t, run(t, write("rootfs.tar", raw)), 2, "no entrypoint or command")
}

// memberEnds returns the offsets where each member of a tar archive ends.
func memberEnds(t *testing.T, b []byte) []int {
	t.Helper()
	var ends []int
	off := 0
	for off+512 <= len(b) {
		hdr := b[off : off+512]
		if bytes.Equal(hdr, make([]byte, 512)) {
			break
		}
		sizeField := strings.TrimRight(strings.TrimSpace(string(hdr[124:136])), "\x00")
		var size int64
		for _, ch := range sizeField {
			if ch >= '0' && ch <= '7' {
				size = size*8 + int64(ch-'0')
			}
		}
		off += 512 + int((size+511)/512*512)
		if off <= len(b) {
			ends = append(ends, off)
		}
	}
	return ends
}

func TestDamagedPrograms(t *testing.T) {
	good := readFile(t, amd64Static(t))
	dirT := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dirT, name)
		os.WriteFile(p, b, 0o755)
		return p
	}
	img := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata})

	// Output of a real failed go build, saved as if it were the binary.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module broken\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() { undefinedCall() }\n"), 0o644)
	cmd := exec.Command("go", "build", "-o", filepath.Join(src, "out"), ".")
	cmd.Dir = src
	failed, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("the broken build succeeded")
	}

	inputs := map[string][]byte{
		"empty":   nil,
		"garbage": bytes.Repeat([]byte{0x13, 0x37, 0xde, 0xad}, 2000),
		"failed":  failed,
		"elfhead": good[:64],
	}
	for _, c := range []int{4, 52, 64, 1000, 4096, len(good) / 4, len(good) / 2, len(good) - 4096, len(good) - 1} {
		inputs["cut-"+strconv.Itoa(c)] = good[:c]
	}
	for name, b := range inputs {
		// As --binary, and as the entrypoint inside the image.
		expectUntrusted(t, "--binary", write(name, b), img)
		inside := writeOCI(t, cfg{arch: "amd64", entry: []string{"/app"}}, []entry{certs, tzdata, file("app", b, 0o755)})
		expectUntrusted(t, inside)
	}
	// The healthy program still passes as --binary, against the same image.
	expect(t, run(t, "--binary", write("good", good), img), 0, "No problems found")
}

func TestUsage(t *testing.T) {
	expect(t, run(t), 2, "give exactly one IMAGE")
	expect(t, run(t, "--fail-on", "maybe", "x.tar"), 2, "--fail-on must be")
	expect(t, run(t, "--bogus", "x.tar"), 2, "unknown flag")
	expect(t, run(t, "--at", "/app", "x.tar"), 2, "--at needs --binary")
	expect(t, run(t, "--version"), 0, "wontexec 0.1.0")
}
