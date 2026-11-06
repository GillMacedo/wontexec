package main

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestVersionOrder(t *testing.T) {
	v := []string{"GLIBC_2.34", "GLIBC_2.2.5", "GLIBC_2.17", "GLIBC_2.3.4", "GLIBC_2.4"}
	sortVersions(v)
	want := "GLIBC_2.2.5 GLIBC_2.3.4 GLIBC_2.4 GLIBC_2.17 GLIBC_2.34"
	if got := strings.Join(v, " "); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	defs := map[string]bool{"GLIBC_2.17": true, "GLIBC_2.30": true, "GLIBC_2.4": true, "GLIBC_PRIVATE": true}
	if got := newestVersion(defs, "GLIBC_2.34"); got != "GLIBC_2.30" {
		t.Errorf("newest = %s", got)
	}
}

func testFS() *imageFS {
	fs := newFS()
	fs.nodes["/bin"] = &fnode{kind: 'l', link: "usr/bin"}
	fs.nodes["/usr/bin/sh"] = &fnode{kind: 'f', mode: 0o755, loaded: true, data: []byte("x"), size: 1}
	fs.nodes["/lib64"] = &fnode{kind: 'l', link: "/usr/lib64"}
	fs.nodes["/usr/lib64/ld.so"] = &fnode{kind: 'l', link: "../lib/real-ld.so"}
	fs.nodes["/usr/lib/real-ld.so"] = &fnode{kind: 'f', mode: 0o755}
	fs.nodes["/loop1"] = &fnode{kind: 'l', link: "loop2"}
	fs.nodes["/loop2"] = &fnode{kind: 'l', link: "/loop1"}
	fs.nodes["/up"] = &fnode{kind: 'l', link: "../../../usr/bin/sh"}
	fs.finish()
	return fs
}

func TestResolve(t *testing.T) {
	fs := testFS()
	cases := map[string]string{
		"/bin/sh":         "/usr/bin/sh",
		"/lib64/ld.so":    "/usr/lib/real-ld.so",
		"/up":             "/usr/bin/sh", // .. can't climb above the image root
		"/usr/../bin//sh": "/usr/bin/sh",
	}
	for in, want := range cases {
		got, n, err := fs.resolve(in)
		if err != nil || n == nil || got != want {
			t.Errorf("resolve(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, _, err := fs.resolve("/loop1"); !errors.Is(err, errLoop) {
		t.Errorf("loop: %v", err)
	}
	if _, _, err := fs.resolve("/bin/sh/x"); !errors.Is(err, errNotDir) {
		t.Errorf("file as dir: %v", err)
	}
	if _, _, err := fs.resolve("/nope"); !errors.Is(err, errNotExist) {
		t.Errorf("missing: %v", err)
	}
	// /usr exists only because it has children.
	if _, n, err := fs.resolve("/usr"); err != nil || n.kind != 'd' {
		t.Errorf("implied dir: %v", err)
	}
}

func TestWalkTarNeedsEndBlock(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"a", "b"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 600, Typeflag: tar.TypeReg})
		tw.Write(bytes.Repeat([]byte{0}, 600)) // zero data, so a cut can look like an end block
	}
	tw.Close()
	whole := buf.Bytes()
	count := func(b []byte) (int, error) {
		n := 0
		err := walkTar(bytes.NewReader(b), nil, "t", func(int, *tar.Header, *tar.Reader) error { n++; return nil })
		return n, err
	}
	if n, err := count(whole); err != nil || n != 2 {
		t.Fatalf("whole archive: %d members, %v", n, err)
	}
	// After the first member (512 header + 1024 data), and inside its zero data.
	for _, cut := range []int{0, 512, 1024, 1536, 2048, 1536 + 512 + 1024} {
		if _, err := count(whole[:cut]); err == nil {
			t.Errorf("cut at %d passed", cut)
		}
	}
}

// TestLdCache builds a small glibc-ld.so.cache1.1 file by hand.
func TestLdCache(t *testing.T) {
	strs := []byte("libfoo.so.1\x00/opt/foo/libfoo.so.1\x00")
	keyOff := uint32(48 + 24)
	valOff := keyOff + uint32(len("libfoo.so.1\x00"))
	var b bytes.Buffer
	b.WriteString("glibc-ld.so.cache1.1")
	binary.Write(&b, binary.LittleEndian, uint32(1))         // nlibs
	binary.Write(&b, binary.LittleEndian, uint32(len(strs))) // len_strings
	b.Write(make([]byte, 48-b.Len()))                        // flags, padding, extension offset, unused
	binary.Write(&b, binary.LittleEndian, int32(0x0303))     // flags
	binary.Write(&b, binary.LittleEndian, keyOff)            // key
	binary.Write(&b, binary.LittleEndian, valOff)            // value
	binary.Write(&b, binary.LittleEndian, uint32(0))         // osversion
	binary.Write(&b, binary.LittleEndian, uint64(0))         // hwcap
	b.Write(strs)
	fs := newFS()
	fs.nodes["/etc/ld.so.cache"] = &fnode{kind: 'f', loaded: true, data: b.Bytes(), size: int64(b.Len())}
	fs.finish()
	c := &checker{img: &image{fs: fs}, rep: &report{}}
	cache := c.ldCache()
	if cache["libfoo.so.1"] != "/opt/foo/libfoo.so.1" {
		t.Errorf("cache = %q", cache)
	}
	// A cut-off cache is reported, not silently used.
	fs.nodes["/etc/ld.so.cache"] = &fnode{kind: 'f', loaded: true, data: b.Bytes()[:60], size: 60}
	c = &checker{img: &image{fs: fs}, rep: &report{}}
	if c.ldCache() != nil || c.rep.Warnings != 1 {
		t.Errorf("cut cache: warnings %d", c.rep.Warnings)
	}
}

func TestUserAndPermissions(t *testing.T) {
	fs := newFS()
	add := func(p, body string) {
		fs.nodes[p] = &fnode{kind: 'f', loaded: true, data: []byte(body), size: int64(len(body))}
	}
	add("/etc/passwd", "root:x:0:0::/root:/bin/sh\napp:x:1000:1000::/home/app:/bin/sh\n")
	add("/etc/group", "root:x:0:\napp:x:1000:\nshared:x:2000:app\n")
	fs.finish()
	c := &checker{img: &image{fs: fs}, rep: &report{}, user: "app"}
	c.checkUser()
	if c.uid != 1000 || c.gid != 1000 || len(c.supp) != 1 || c.supp[0] != 2000 {
		t.Fatalf("app: uid %d gid %d groups %v", c.uid, c.gid, c.supp)
	}
	file := func(mode int64, uid, gid int) *fnode {
		return &fnode{kind: 'f', mode: mode, uid: uid, gid: gid, ownerKnown: true}
	}
	checks := []struct {
		n    *fnode
		want bool
	}{
		{file(0o700, 1000, 1000), true},
		{file(0o070, 1000, 1000), false}, // the owner bits win for the owner
		{file(0o750, 0, 2000), true},     // supplementary group
		{file(0o750, 0, 3000), false},
		{file(0o751, 0, 3000), true},
	}
	for i, ch := range checks {
		if got := c.canExec(ch.n); got != ch.want {
			t.Errorf("case %d: canExec = %v", i, got)
		}
	}
	c = &checker{img: &image{fs: fs}, rep: &report{}, user: "app:nogroup"}
	c.checkUser()
	if c.rep.Errors != 1 {
		t.Errorf("unknown group: %d errors", c.rep.Errors)
	}
}
