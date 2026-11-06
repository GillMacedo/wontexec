// Command wontexec checks, without running anything, whether a Linux program
// will start inside a container image: the file and its permissions, the CPU
// architecture, the ELF loader, shared libraries and glibc symbol versions,
// the image user, and for Go programs the CA certificates and time zone data.
//
// Usage:
//
//	wontexec [--binary FILE [--at PATH]] [--exec PATH] [--platform OS/ARCH] IMAGE
//
// IMAGE is a docker save tar, an OCI layout (tar or directory), a root
// filesystem tar (optionally gzip), or a root filesystem directory.
//
// It uses only the standard library, so "go run wontexec.go" works anywhere.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const version = "0.1.0"

// untrusted marks an input that can't be read fully. It always exits 2.
type untrusted struct{ msg string }

func (u *untrusted) Error() string { return u.msg }

func distrust(format string, args ...any) error {
	return &untrusted{msg: fmt.Sprintf(format, args...)}
}

// ---------- image filesystem ----------

type fnode struct {
	kind       byte // 'f' file, 'd' dir, 'l' symlink
	mode       int64
	uid, gid   int
	ownerKnown bool
	link       string
	size       int64
	data       []byte
	loaded     bool
	load       func() ([]byte, error)
}

type imageFS struct {
	nodes map[string]*fnode
	dirs  map[string]bool // directories implied by their children
}

func newFS() *imageFS {
	return &imageFS{nodes: map[string]*fnode{"/": {kind: 'd', mode: 0o755, ownerKnown: true}}, dirs: map[string]bool{}}
}

func cleanPath(p string) string {
	return path.Clean("/" + p)
}

func (f *imageFS) removeTree(p string) {
	delete(f.nodes, p)
	prefix := p + "/"
	if p == "/" {
		prefix = "/"
	}
	for k := range f.nodes {
		if strings.HasPrefix(k, prefix) && k != "/" {
			delete(f.nodes, k)
		}
	}
}

func (f *imageFS) finish() {
	f.dirs = map[string]bool{}
	for p := range f.nodes {
		for d := path.Dir(p); ; d = path.Dir(d) {
			f.dirs[d] = true
			if d == "/" {
				break
			}
		}
	}
}

func (n *fnode) content() ([]byte, error) {
	if n.loaded {
		return n.data, nil
	}
	if n.load == nil {
		return nil, distrust("file content is not available")
	}
	b, err := n.load()
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n.size {
		return nil, distrust("file changed size while reading it (%d bytes, expected %d)", len(b), n.size)
	}
	n.data, n.loaded = b, true
	return b, nil
}

var errNotExist = errors.New("no such file or directory")
var errNotDir = errors.New("not a directory")
var errLoop = errors.New("too many levels of symbolic links")

// resolve follows symlinks inside the image, the way the kernel would with
// the image as root. It returns the final path and node.
func (f *imageFS) resolve(p string) (string, *fnode, error) {
	return f.resolveN(p, 0)
}

func (f *imageFS) resolveN(p string, depth int) (string, *fnode, error) {
	if depth > 40 {
		return "", nil, errLoop
	}
	p = cleanPath(p)
	if p == "/" {
		return "/", f.nodes["/"], nil
	}
	parts := strings.Split(p[1:], "/")
	cur := "/"
	for i, part := range parts {
		next := path.Join(cur, part)
		n, ok := f.nodes[next]
		last := i == len(parts)-1
		if !ok {
			if f.dirs[next] {
				if last {
					return next, &fnode{kind: 'd', mode: 0o755}, nil
				}
				cur = next
				continue
			}
			return next, nil, errNotExist
		}
		switch n.kind {
		case 'l':
			target := n.link
			if !strings.HasPrefix(target, "/") {
				target = path.Join(cur, target)
			}
			rest := strings.Join(parts[i+1:], "/")
			return f.resolveN(path.Join(target, rest), depth+1)
		case 'f':
			if !last {
				return next, nil, errNotDir
			}
			return next, n, nil
		case 'd':
			if last {
				return next, n, nil
			}
			cur = next
		}
	}
	return cur, f.nodes[cur], nil
}

func (f *imageFS) readFile(p string) ([]byte, error) {
	_, n, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	if n.kind != 'f' {
		return nil, errors.New("not a regular file")
	}
	return n.content()
}

func (f *imageFS) exists(p string) bool {
	_, n, err := f.resolve(p)
	return err == nil && n != nil
}

// dirHasFiles reports whether a directory in the image holds at least one entry.
func (f *imageFS) dirHasFiles(p string) bool {
	rp, n, err := f.resolve(p)
	if err != nil || n.kind != 'd' {
		return false
	}
	prefix := rp + "/"
	for k, v := range f.nodes {
		if strings.HasPrefix(k, prefix) && v.kind != 'd' {
			return true
		}
	}
	return false
}

// ---------- reading tar streams safely ----------

// trackReader counts and hashes every byte, and captures the 512 bytes at a
// mark so the tar end-of-archive block can be checked. archive/tar returns a
// clean io.EOF when a stream is cut exactly at a member boundary.
type trackReader struct {
	r       io.Reader
	n       int64
	h       hash.Hash
	mark    int64
	endBlk  [512]byte
	endSeen int
}

func (t *trackReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		if t.h != nil {
			t.h.Write(p[:n])
		}
		start, end := t.n, t.n+int64(n)
		lo, hi := max(start, t.mark), min(end, t.mark+512)
		if lo < hi {
			copy(t.endBlk[lo-t.mark:], p[lo-start:hi-start])
			if int(hi-t.mark) > t.endSeen {
				t.endSeen = int(hi - t.mark)
			}
		}
		t.n = end
	}
	return n, err
}

func (t *trackReader) setMark(m int64) {
	t.mark = m
	t.endSeen = 0
}

// walkTar calls fn for each member of an uncompressed tar stream and checks
// that the stream ends with a proper end-of-archive block. It reads the
// stream to the end so the hash covers all of it.
func walkTar(r io.Reader, h hash.Hash, what string, fn func(i int, hdr *tar.Header, tr *tar.Reader) error) error {
	t := &trackReader{r: r, h: h}
	tr := tar.NewReader(t)
	i := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return distrust("%s is not a readable tar archive (%v): it may be empty, cut off or not a tar at all", what, err)
		}
		dataStart := t.n
		size := hdr.Size
		if hdr.Typeflag == tar.TypeGNUSparse || len(hdr.PAXRecords) > 0 && hdr.PAXRecords["GNU.sparse.major"] != "" {
			return distrust("%s has a sparse file (%s), which wontexec doesn't read", what, hdr.Name)
		}
		if err := fn(i, hdr, tr); err != nil {
			return err
		}
		i++
		t.setMark(dataStart + (size+511)/512*512)
	}
	if t.endSeen < 512 || !bytes.Equal(t.endBlk[:], make([]byte, 512)) {
		if t.n == 0 {
			return distrust("%s is empty", what)
		}
		return distrust("%s ends without a tar end-of-archive block, so it was probably cut off", what)
	}
	if _, err := io.Copy(io.Discard, t); err != nil {
		return distrust("%s: reading past the end of the archive: %v", what, err)
	}
	return nil
}

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// decompress wraps a stream that may be gzip compressed.
func decompress(r io.Reader, what string) (io.Reader, func() error, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(4)
	if bytes.HasPrefix(head, zstdMagic) {
		return nil, nil, distrust("%s is zstd compressed, which wontexec doesn't read; save the image with gzip or uncompressed layers", what)
	}
	if bytes.HasPrefix(head, gzipMagic) {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, distrust("%s: bad gzip data: %v", what, err)
		}
		return zr, zr.Close, nil
	}
	return br, func() error { return nil }, nil
}

const keepInMemory = 1 << 16

// applyLayer reads one layer (or a whole root filesystem tar) onto fs.
// open must return the raw stream each time it is called. wantDiff, when set,
// is the sha256 the uncompressed stream must have.
func applyLayer(fs *imageFS, open func() (io.ReadCloser, error), what, wantDiff string) error {
	rc, err := open()
	if err != nil {
		return distrust("%s: %v", what, err)
	}
	defer rc.Close()
	r, closeZ, err := decompress(rc, what)
	if err != nil {
		return err
	}
	h := sha256.New()
	hardlinks := map[string]string{}
	err = walkTar(r, h, what, func(i int, hdr *tar.Header, tr *tar.Reader) error {
		p := cleanPath(hdr.Name)
		dir, base := path.Split(p)
		dir = cleanPath(dir)
		if base == ".wh..wh..opq" {
			for k := range fs.nodes {
				if strings.HasPrefix(k, dir+"/") && dir != "/" || dir == "/" && k != "/" {
					delete(fs.nodes, k)
				}
			}
			return nil
		}
		if strings.HasPrefix(base, ".wh.") {
			fs.removeTree(path.Join(dir, strings.TrimPrefix(base, ".wh.")))
			return nil
		}
		n := &fnode{mode: hdr.Mode & 0o7777, uid: hdr.Uid, gid: hdr.Gid, ownerKnown: true}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if old, ok := fs.nodes[p]; ok && old.kind != 'd' {
				fs.removeTree(p)
			}
			n.kind = 'd'
			fs.nodes[p] = n
			return nil
		case tar.TypeSymlink:
			n.kind, n.link = 'l', hdr.Linkname
		case tar.TypeLink:
			hardlinks[p] = cleanPath(hdr.Linkname)
			return nil
		case tar.TypeReg, tar.TypeRegA:
			n.kind, n.size = 'f', hdr.Size
			if hdr.Size <= keepInMemory || strings.Contains(base, ".so") {
				b, err := io.ReadAll(tr)
				if err != nil {
					return distrust("%s: reading %s: %v (the archive may be cut off)", what, p, err)
				}
				n.data, n.loaded = b, true
			} else {
				idx := i
				n.load = func() ([]byte, error) { return readMember(open, what, idx) }
			}
		default:
			// Devices, fifos and the like can't be programs or libraries.
			n.kind = 'f'
		}
		fs.removeTree(p)
		fs.nodes[p] = n
		return nil
	})
	if err != nil {
		return err
	}
	if err := closeZ(); err != nil {
		return distrust("%s: %v", what, err)
	}
	for p, target := range hardlinks {
		t, ok := fs.nodes[target]
		if !ok {
			return distrust("%s: hard link %s points to %s, which isn't in the archive", what, p, target)
		}
		fs.removeTree(p)
		cp := *t
		fs.nodes[p] = &cp
	}
	if wantDiff != "" {
		got := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if got != wantDiff {
			return distrust("%s: content digest %s doesn't match the image config's diff_id %s, so the layer is damaged or cut off", what, got, wantDiff)
		}
	}
	return nil
}

// readMember re-reads a stream to fetch the content of member idx.
func readMember(open func() (io.ReadCloser, error), what string, idx int) ([]byte, error) {
	rc, err := open()
	if err != nil {
		return nil, distrust("%s: %v", what, err)
	}
	defer rc.Close()
	r, _, err := decompress(rc, what)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(r)
	for i := 0; ; i++ {
		_, err := tr.Next()
		if err != nil {
			return nil, distrust("%s: re-reading member %d: %v", what, idx, err)
		}
		if i == idx {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, distrust("%s: re-reading member %d: %v", what, idx, err)
			}
			return b, nil
		}
	}
}

// ---------- image formats ----------

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant"`
	Config       struct {
		User       string   `json:"User"`
		Env        []string `json:"Env"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type image struct {
	fs        *imageFS
	cfg       *imageConfig
	format    string
	layers    int
	platforms []string
}

// archive gives random access to members of an uncompressed outer tar file.
type archive struct {
	f       *os.File
	members map[string]*tar.Header
	offsets map[string]int64
}

func openArchive(name string) (*archive, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	a := &archive{f: f, members: map[string]*tar.Header{}, offsets: map[string]int64{}}
	t := &trackReader{r: f}
	tr := tar.NewReader(t)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		p := strings.TrimPrefix(cleanPath(hdr.Name), "/")
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			a.members[p] = hdr
			a.offsets[p] = t.n
		} else if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			a.members[p] = hdr
		}
		t.setMark(t.n + (hdr.Size+511)/512*512)
	}
	if t.endSeen < 512 || !bytes.Equal(t.endBlk[:], make([]byte, 512)) {
		return nil, errors.New("no end-of-archive block")
	}
	return a, nil
}

func (a *archive) has(p string) bool { _, ok := a.members[p]; return ok }

// hasDir reports whether any file or link in the archive is under directory p.
func (a *archive) hasDir(p string) bool {
	for m := range a.members {
		if strings.HasPrefix(m, p+"/") {
			return true
		}
	}
	return false
}

// partialLayout is the error for an image layout with some of its parts missing,
// such as a copy that stopped early. Reading it as a root filesystem would check
// an empty image and could print a clean result.
func partialLayout(name string) error {
	return distrust("%s looks like an OCI image layout or docker save archive, but index.json, oci-layout or manifest.json is missing: it may be an incomplete copy", name)
}

func (a *archive) opener(p string) (func() (io.ReadCloser, error), error) {
	for i := 0; i < 10; i++ {
		hdr, ok := a.members[p]
		if !ok {
			return nil, fmt.Errorf("%s is missing from the archive", p)
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			l := hdr.Linkname
			if hdr.Typeflag == tar.TypeSymlink && !strings.HasPrefix(l, "/") {
				l = path.Join(path.Dir(p), l)
			}
			p = strings.TrimPrefix(cleanPath(l), "/")
			continue
		}
		off, size := a.offsets[p], hdr.Size
		return func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(a.f, off, size)), nil
		}, nil
	}
	return nil, fmt.Errorf("%s: too many links", p)
}

func (a *archive) read(p string, limit int64) ([]byte, error) {
	open, err := a.opener(p)
	if err != nil {
		return nil, err
	}
	rc, _ := open()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return b, nil
}

// blobStore is the common shape of an OCI layout or docker save, in a tar or a directory.
type blobStore interface {
	has(p string) bool
	read(p string, limit int64) ([]byte, error)
	opener(p string) (func() (io.ReadCloser, error), error)
}

type dirStore struct{ root string }

func (d dirStore) full(p string) string { return filepath.Join(d.root, filepath.FromSlash(p)) }
func (d dirStore) has(p string) bool {
	_, err := os.Stat(d.full(p))
	return err == nil
}
func (d dirStore) read(p string, limit int64) ([]byte, error) {
	fi, err := os.Stat(d.full(p))
	if err != nil {
		return nil, err
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return os.ReadFile(d.full(p))
}
func (d dirStore) opener(p string) (func() (io.ReadCloser, error), error) {
	if !d.has(p) {
		return nil, fmt.Errorf("%s is missing", p)
	}
	return func() (io.ReadCloser, error) { return os.Open(d.full(p)) }, nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type ociIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
	Config        *descriptor  `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func blobPath(digest string) (string, error) {
	algo, hexd, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" || len(hexd) != 64 {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	if _, err := hex.DecodeString(hexd); err != nil {
		return "", fmt.Errorf("bad digest %q", digest)
	}
	return "blobs/sha256/" + hexd, nil
}

func readBlob(s blobStore, d descriptor, limit int64) ([]byte, error) {
	p, err := blobPath(d.Digest)
	if err != nil {
		return nil, distrust("%v", err)
	}
	b, err := s.read(p, limit)
	if err != nil {
		return nil, distrust("blob %s: %v", d.Digest, err)
	}
	sum := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(sum[:]) != d.Digest {
		return nil, distrust("blob %s doesn't match its digest, so it is damaged or cut off", d.Digest)
	}
	return b, nil
}

func platformOf(c *imageConfig) string {
	p := c.OS + "/" + c.Architecture
	if c.Variant != "" {
		p += "/" + c.Variant
	}
	return p
}

func platformMatch(want, os_, arch, variant string) bool {
	parts := strings.Split(want, "/")
	if len(parts) < 2 || parts[0] != os_ || parts[1] != arch {
		return false
	}
	return len(parts) < 3 || parts[2] == variant
}

func strictJSON(b []byte, v any, what string) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return distrust("%s is not valid JSON: %v", what, err)
	}
	if dec.More() {
		return distrust("%s has data after the JSON value", what)
	}
	return nil
}

// loadOCI reads an OCI image layout through index.json.
func loadOCI(s blobStore, platform string) (*image, error) {
	b, err := s.read("index.json", 16<<20)
	if err != nil {
		return nil, distrust("index.json: %v", err)
	}
	var idx ociIndex
	if err := strictJSON(b, &idx, "index.json"); err != nil {
		return nil, err
	}
	var all []string
	for depth := 0; depth < 4; depth++ {
		var cands []descriptor
		for _, m := range idx.Manifests {
			if m.Platform != nil && (m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown") {
				continue // attestation manifests
			}
			cands = append(cands, m)
		}
		if len(cands) == 0 {
			return nil, distrust("the image index lists no image manifests")
		}
		var pick *descriptor
		if len(cands) == 1 {
			pick = &cands[0]
		} else {
			all = nil
			for i := range cands {
				c := cands[i]
				if c.Platform == nil {
					continue
				}
				all = append(all, platformOf(&imageConfig{OS: c.Platform.OS, Architecture: c.Platform.Architecture, Variant: c.Platform.Variant}))
				if platform != "" && platformMatch(platform, c.Platform.OS, c.Platform.Architecture, c.Platform.Variant) && pick == nil {
					pick = &cands[i]
				}
			}
			if pick == nil {
				if platform == "" {
					return nil, distrust("the image has several platforms (%s); pass --platform", strings.Join(all, ", "))
				}
				return nil, distrust("the image has no %s platform (it has %s)", platform, strings.Join(all, ", "))
			}
		}
		mb, err := readBlob(s, *pick, 16<<20)
		if err != nil {
			return nil, err
		}
		var m ociIndex
		if err := strictJSON(mb, &m, "manifest "+pick.Digest); err != nil {
			return nil, err
		}
		if len(m.Manifests) > 0 && m.Config == nil {
			idx = m
			continue
		}
		if m.Config == nil {
			return nil, distrust("manifest %s has no config", pick.Digest)
		}
		img, err := loadManifest(s, *m.Config, m.Layers, platform)
		if err != nil {
			return nil, err
		}
		img.format = "OCI layout"
		img.platforms = all
		return img, nil
	}
	return nil, distrust("the image index nests too deeply")
}

func loadConfig(b []byte, what string) (*imageConfig, error) {
	var cfg imageConfig
	if err := strictJSON(b, &cfg, what); err != nil {
		return nil, err
	}
	if cfg.RootFS.Type != "layers" {
		return nil, distrust("%s has no rootfs layers list, so it isn't an image config", what)
	}
	return &cfg, nil
}

func loadManifest(s blobStore, cfgDesc descriptor, layers []descriptor, platform string) (*image, error) {
	cb, err := readBlob(s, cfgDesc, 16<<20)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(cb, "image config "+cfgDesc.Digest)
	if err != nil {
		return nil, err
	}
	if platform != "" && !platformMatch(platform, cfg.OS, cfg.Architecture, cfg.Variant) {
		return nil, distrust("the image is %s, not %s", platformOf(cfg), platform)
	}
	if len(cfg.RootFS.DiffIDs) != len(layers) {
		return nil, distrust("the manifest lists %d layers but the config lists %d", len(layers), len(cfg.RootFS.DiffIDs))
	}
	fs := newFS()
	for i, l := range layers {
		p, err := blobPath(l.Digest)
		if err != nil {
			return nil, distrust("%v", err)
		}
		raw, err := s.opener(p)
		if err != nil {
			return nil, distrust("layer %d: %v", i+1, err)
		}
		// Check the compressed digest while reading.
		want := l.Digest
		var gotRaw hash.Hash
		open := func() (io.ReadCloser, error) {
			rc, err := raw()
			if err != nil {
				return nil, err
			}
			gotRaw = sha256.New()
			return struct {
				io.Reader
				io.Closer
			}{io.TeeReader(rc, gotRaw), rc}, nil
		}
		what := fmt.Sprintf("layer %d (%s)", i+1, short(l.Digest))
		if err := applyLayer(fs, open, what, cfg.RootFS.DiffIDs[i]); err != nil {
			return nil, err
		}
		if "sha256:"+hex.EncodeToString(gotRaw.Sum(nil)) != want {
			return nil, distrust("%s doesn't match its digest, so it is damaged or cut off", what)
		}
	}
	fs.finish()
	return &image{fs: fs, cfg: cfg, layers: len(layers)}, nil
}

func short(d string) string {
	_, h, _ := strings.Cut(d, ":")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// loadDockerSave reads the manifest.json written by docker save.
func loadDockerSave(s blobStore, platform string) (*image, error) {
	b, err := s.read("manifest.json", 16<<20)
	if err != nil {
		return nil, distrust("manifest.json: %v", err)
	}
	var ms []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := strictJSON(b, &ms, "manifest.json"); err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, distrust("manifest.json lists no images")
	}
	type cand struct {
		i   int
		cfg *imageConfig
	}
	var cands []cand
	var plats []string
	for i, m := range ms {
		cb, err := s.read(strings.TrimPrefix(cleanPath(m.Config), "/"), 16<<20)
		if err != nil {
			return nil, distrust("image config %s: %v", m.Config, err)
		}
		cfg, err := loadConfig(cb, "image config "+m.Config)
		if err != nil {
			return nil, err
		}
		plats = append(plats, platformOf(cfg))
		if platform == "" || platformMatch(platform, cfg.OS, cfg.Architecture, cfg.Variant) {
			cands = append(cands, cand{i, cfg})
		}
	}
	if len(cands) == 0 {
		return nil, distrust("the archive has no %s image (it has %s)", platform, strings.Join(plats, ", "))
	}
	if len(cands) > 1 {
		return nil, distrust("the archive holds %d images; save one image per file", len(cands))
	}
	m, cfg := ms[cands[0].i], cands[0].cfg
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return nil, distrust("manifest.json lists %d layers but the config lists %d", len(m.Layers), len(cfg.RootFS.DiffIDs))
	}
	fs := newFS()
	for i, l := range m.Layers {
		open, err := s.opener(strings.TrimPrefix(cleanPath(l), "/"))
		if err != nil {
			return nil, distrust("layer %d: %v", i+1, err)
		}
		if err := applyLayer(fs, open, fmt.Sprintf("layer %d (%s)", i+1, l), cfg.RootFS.DiffIDs[i]); err != nil {
			return nil, err
		}
	}
	fs.finish()
	return &image{fs: fs, cfg: cfg, format: "docker save", layers: len(m.Layers)}, nil
}

// loadRootDir reads an unpacked root filesystem. Owners aren't read, so the
// permission check only looks at mode bits.
func loadRootDir(root string) (*image, error) {
	fs := newFS()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return distrust("reading %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		ip := cleanPath(filepath.ToSlash(rel))
		if ip == "/" {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return distrust("reading %s: %v", p, err)
		}
		n := &fnode{mode: int64(fi.Mode().Perm())}
		if fi.Mode()&os.ModeSetuid != 0 {
			n.mode |= 0o4000
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return distrust("reading link %s: %v", p, err)
			}
			n.kind, n.link = 'l', filepath.ToSlash(l)
		case fi.IsDir():
			n.kind = 'd'
		default:
			n.kind, n.size = 'f', fi.Size()
			full := p
			n.load = func() ([]byte, error) { return os.ReadFile(full) }
		}
		fs.nodes[ip] = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	fs.finish()
	return &image{fs: fs, format: "root filesystem directory", layers: 1}, nil
}

func loadImage(name, platform string) (*image, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, distrust("%v", err)
	}
	if fi.IsDir() {
		d := dirStore{name}
		if d.has("index.json") && d.has("oci-layout") {
			return loadOCI(d, platform)
		}
		if d.has("manifest.json") && !d.has("etc") {
			return loadDockerSave(d, platform)
		}
		if (d.has("oci-layout") || d.has("index.json") || d.has("blobs")) && !d.has("etc") && !d.has("bin") && !d.has("usr") {
			return nil, partialLayout(name)
		}
		return loadRootDir(name)
	}
	if fi.Size() == 0 {
		return nil, distrust("%s is empty", name)
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, distrust("%v", err)
	}
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	f.Close()
	head = head[:n]
	if bytes.HasPrefix(head, gzipMagic) || bytes.HasPrefix(head, zstdMagic) {
		return loadRootTar(name)
	}
	a, err := openArchive(name)
	if err != nil {
		return nil, distrust("%s is not a readable tar archive (%v): it may be empty, cut off, or the output of a failed command", name, err)
	}
	if a.has("index.json") && a.has("oci-layout") {
		img, err := loadOCI(a, platform)
		return img, err
	}
	if a.has("manifest.json") && !a.has("etc/passwd") && !a.has("bin") {
		return loadDockerSave(a, platform)
	}
	if a.has("oci-layout") || a.has("index.json") || a.hasDir("blobs") {
		if !a.hasDir("etc") && !a.hasDir("bin") && !a.hasDir("usr") {
			a.f.Close()
			return nil, partialLayout(name)
		}
	}
	a.f.Close()
	return loadRootTar(name)
}

func loadRootTar(name string) (*image, error) {
	fs := newFS()
	open := func() (io.ReadCloser, error) { return os.Open(name) }
	if err := applyLayer(fs, open, name, ""); err != nil {
		return nil, err
	}
	fs.finish()
	return &image{fs: fs, format: "root filesystem tar", layers: 1}, nil
}

// ---------- ELF ----------

type verNeed struct {
	file     string
	versions []string
	weak     map[string]bool
}

type elfObj struct {
	path     string
	machine  elf.Machine
	class    elf.Class
	data     elf.Data
	typ      elf.Type
	interp   string
	dynamic  bool
	needed   []string
	runpath  []string
	rpath    []string
	verneed  []verNeed
	verdef   map[string]bool
	hasVer   bool
	sections map[string][]byte
}

func parseELF(b []byte, name string) (*elfObj, error) {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, distrust("%s: not a readable ELF file: %v", name, err)
	}
	size := uint64(len(b))
	for _, p := range f.Progs {
		if p.Filesz > 0 && (p.Off > size || p.Filesz > size-p.Off) {
			return nil, distrust("%s is cut off: a program segment needs bytes %d to %d but the file has %d", name, p.Off, p.Off+p.Filesz, size)
		}
	}
	for _, s := range f.Sections {
		if s.Type != elf.SHT_NOBITS && s.FileSize > 0 && (s.Offset > size || s.FileSize > size-s.Offset) {
			return nil, distrust("%s is cut off: section %s needs bytes %d to %d but the file has %d", name, s.Name, s.Offset, s.Offset+s.FileSize, size)
		}
	}
	o := &elfObj{path: name, machine: f.Machine, class: f.Class, data: f.Data, typ: f.Type, verdef: map[string]bool{}, sections: map[string][]byte{}}
	for _, p := range f.Progs {
		switch p.Type {
		case elf.PT_INTERP:
			buf := make([]byte, p.Filesz)
			if _, err := p.ReadAt(buf, 0); err != nil {
				return nil, distrust("%s: reading the interpreter path: %v", name, err)
			}
			o.interp = string(bytes.TrimRight(buf, "\x00"))
		case elf.PT_DYNAMIC:
			o.dynamic = true
		}
	}
	if o.dynamic {
		if f.Section(".dynamic") == nil && f.SectionByType(elf.SHT_DYNAMIC) == nil {
			return nil, distrust("%s is dynamically linked but has no section headers to read its library list from", name)
		}
		if o.needed, err = f.DynString(elf.DT_NEEDED); err != nil {
			return nil, distrust("%s: reading DT_NEEDED: %v", name, err)
		}
		for _, tag := range []elf.DynTag{elf.DT_RUNPATH, elf.DT_RPATH} {
			vals, err := f.DynString(tag)
			if err != nil {
				return nil, distrust("%s: reading %v: %v", name, tag, err)
			}
			var dirs []string
			for _, v := range vals {
				dirs = append(dirs, strings.Split(v, ":")...)
			}
			if tag == elf.DT_RUNPATH {
				o.runpath = dirs
			} else {
				o.rpath = dirs
			}
		}
		if err := o.readVersions(f, b); err != nil {
			return nil, err
		}
	}
	// PIE builds by older Go versions name these .data.rel.ro.gopclntab and so on.
	for _, name := range []string{".gopclntab", ".go.buildinfo"} {
		for _, s := range f.Sections {
			if (s.Name == name || strings.HasSuffix(s.Name, name)) && s.Type != elf.SHT_NOBITS {
				d, err := s.Data()
				if err != nil {
					return nil, distrust("%s: reading %s: %v", o.path, s.Name, err)
				}
				o.sections[name] = d
				break
			}
		}
	}
	return o, nil
}

func cstr(tab []byte, off uint32) (string, bool) {
	if int(off) >= len(tab) {
		return "", false
	}
	end := bytes.IndexByte(tab[off:], 0)
	if end < 0 {
		return "", false
	}
	return string(tab[off : int(off)+end]), true
}

func (o *elfObj) readVersions(f *elf.File, b []byte) error {
	bo := f.ByteOrder
	for _, s := range f.Sections {
		if s.Type != elf.SHT_GNU_VERNEED && s.Type != elf.SHT_GNU_VERDEF {
			continue
		}
		if int(s.Link) >= len(f.Sections) {
			return distrust("%s: bad version section link", o.path)
		}
		strtab, err := f.Sections[s.Link].Data()
		if err != nil {
			return distrust("%s: reading version strings: %v", o.path, err)
		}
		d, err := s.Data()
		if err != nil {
			return distrust("%s: reading %s: %v", o.path, s.Name, err)
		}
		bad := distrust("%s: section %s is malformed", o.path, s.Name)
		off := 0
		for count := 0; count < 10000; count++ {
			if off < 0 || off+16 > len(d) {
				return bad
			}
			if s.Type == elf.SHT_GNU_VERNEED {
				cnt := int(bo.Uint16(d[off+2:]))
				file, ok := cstr(strtab, bo.Uint32(d[off+4:]))
				if !ok {
					return bad
				}
				vn := verNeed{file: file, weak: map[string]bool{}}
				aux := off + int(bo.Uint32(d[off+8:]))
				for j := 0; j < cnt; j++ {
					if aux < 0 || aux+16 > len(d) {
						return bad
					}
					flags := bo.Uint16(d[aux+4:])
					nm, ok := cstr(strtab, bo.Uint32(d[aux+8:]))
					if !ok {
						return bad
					}
					vn.versions = append(vn.versions, nm)
					if flags&0x2 != 0 {
						vn.weak[nm] = true
					}
					aux += int(bo.Uint32(d[aux+12:]))
				}
				o.verneed = append(o.verneed, vn)
				next := bo.Uint32(d[off+12:])
				if next == 0 {
					break
				}
				off += int(next)
			} else {
				if off+20 > len(d) {
					return bad
				}
				flags := bo.Uint16(d[off+2:])
				cnt := bo.Uint16(d[off+6:])
				aux := off + int(bo.Uint32(d[off+12:]))
				if cnt > 0 {
					if aux < 0 || aux+8 > len(d) {
						return bad
					}
					nm, ok := cstr(strtab, bo.Uint32(d[aux:]))
					if !ok {
						return bad
					}
					if flags&0x1 == 0 {
						o.verdef[nm] = true
					}
				}
				o.hasVer = true
				next := bo.Uint32(d[off+16:])
				if next == 0 {
					break
				}
				off += int(next)
			}
		}
	}
	return nil
}

var archNames = map[elf.Machine]string{
	elf.EM_X86_64:    "amd64",
	elf.EM_AARCH64:   "arm64",
	elf.EM_386:       "386",
	elf.EM_ARM:       "arm",
	elf.EM_PPC64:     "ppc64le",
	elf.EM_S390:      "s390x",
	elf.EM_RISCV:     "riscv64",
	elf.EM_LOONGARCH: "loong64",
	elf.EM_MIPS:      "mips",
}

func (o *elfObj) arch() string {
	a, ok := archNames[o.machine]
	if !ok {
		return o.machine.String()
	}
	switch {
	case o.machine == elf.EM_PPC64 && o.data == elf.ELFDATA2MSB:
		return "ppc64"
	case o.machine == elf.EM_MIPS && o.class == elf.ELFCLASS64:
		if o.data == elf.ELFDATA2LSB {
			return "mips64le"
		}
		return "mips64"
	case o.machine == elf.EM_MIPS && o.data == elf.ELFDATA2LSB:
		return "mipsle"
	}
	return a
}

var triplets = map[string]string{
	"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu", "386": "i386-linux-gnu",
	"arm": "arm-linux-gnueabihf", "ppc64le": "powerpc64le-linux-gnu", "s390x": "s390x-linux-gnu",
	"riscv64": "riscv64-linux-gnu", "mips64le": "mips64el-linux-gnuabi64", "loong64": "loongarch64-linux-gnu",
}

// ---------- the check ----------

type finding struct {
	Check   string   `json:"check"`
	Level   string   `json:"level"` // ok, skip, warn, error
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

type goInfo struct {
	Version   string `json:"version"`
	CGO       string `json:"cgo_enabled,omitempty"`
	Module    string `json:"module,omitempty"`
	GOOS      string `json:"goos,omitempty"`
	GOARCH    string `json:"goarch,omitempty"`
	BuildMode string `json:"buildmode,omitempty"`
	ldflags   string
	pcln      []byte
	fallback  bool
}

type report struct {
	Image    string    `json:"image"`
	Format   string    `json:"format"`
	Platform string    `json:"platform,omitempty"`
	Layers   int       `json:"layers"`
	Program  string    `json:"program"`
	Source   string    `json:"source"`
	Go       *goInfo   `json:"go,omitempty"`
	Findings []finding `json:"findings"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
	Skipped  int       `json:"skipped"`
}

type checker struct {
	img    *image
	rep    *report
	env    map[string]string
	arch   string // image architecture, "" if unknown
	user   string
	libc   string // "glibc", "musl" or ""
	seen   map[string]bool
	cache  map[string]string // ld.so.cache name -> path
	cached bool
	fatal  error // set when an input turns out to be unreadable

	uid, gid  int
	supp      []int
	userKnown bool
}

func (c *checker) add(check, level, msg string, details ...string) {
	c.rep.Findings = append(c.rep.Findings, finding{Check: check, Level: level, Message: msg, Details: details})
	switch level {
	case "error":
		c.rep.Errors++
	case "warn":
		c.rep.Warnings++
	case "skip":
		c.rep.Skipped++
	}
}

type options struct {
	binary, at, exec, platform, user, format, failOn string
}

func run(imageName string, opt options) (*report, error) {
	img, err := loadImage(imageName, opt.platform)
	if err != nil {
		return nil, err
	}
	rep := &report{Image: imageName, Format: img.format, Layers: img.layers}
	c := &checker{img: img, rep: rep, env: map[string]string{}, seen: map[string]bool{}}
	if img.cfg != nil {
		for _, e := range img.cfg.Config.Env {
			k, v, _ := strings.Cut(e, "=")
			c.env[k] = v
		}
		if img.cfg.Architecture != "" {
			c.arch = img.cfg.Architecture
			rep.Platform = platformOf(img.cfg)
			if img.cfg.OS != "" && img.cfg.OS != "linux" {
				return nil, distrust("the image is for %s; wontexec checks Linux images only", img.cfg.OS)
			}
		}
		c.user = img.cfg.Config.User
	}
	if c.arch == "" && opt.platform != "" {
		parts := strings.Split(opt.platform, "/")
		if len(parts) >= 2 {
			c.arch = parts[1]
			rep.Platform = opt.platform
		}
	}
	if opt.user != "" {
		c.user = opt.user
	}
	if _, ok := c.env["PATH"]; !ok {
		c.env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	// Decide which program to check.
	var argv0 string
	shellCmd := false
	if img.cfg != nil {
		argv := append(append([]string{}, img.cfg.Config.Entrypoint...), img.cfg.Config.Cmd...)
		if len(img.cfg.Config.Entrypoint) > 0 {
			argv = img.cfg.Config.Entrypoint
		}
		if len(argv) > 0 {
			argv0 = argv[0]
		}
		if len(argv) > 1 && argv[1] == "-c" && opt.exec == "" && opt.binary == "" {
			shellCmd = true
		}
	}
	if opt.exec != "" {
		argv0 = opt.exec
	}
	if opt.binary != "" {
		b, err := os.ReadFile(opt.binary)
		if err != nil {
			return nil, distrust("%v", err)
		}
		if len(b) == 0 {
			return nil, distrust("%s is empty, so there is nothing to check (a failed build?)", opt.binary)
		}
		fi, _ := os.Stat(opt.binary)
		at := opt.at
		if at == "" {
			if strings.HasPrefix(argv0, "/") {
				at = argv0
			} else {
				at = "/" + filepath.Base(opt.binary)
			}
		}
		at = cleanPath(at)
		mode := int64(fi.Mode().Perm())
		if runtime.GOOS == "windows" {
			// Windows has no exec bit; COPY in a Dockerfile keeps 0755 from the build context there.
			mode = 0o755
		}
		img.fs.removeTree(at)
		img.fs.nodes[at] = &fnode{kind: 'f', mode: mode, ownerKnown: true, size: int64(len(b)), data: b, loaded: true}
		img.fs.finish()
		argv0 = at
		rep.Source = "--binary " + opt.binary
	} else if opt.exec != "" {
		rep.Source = "--exec"
	} else if argv0 != "" {
		rep.Source = "image entrypoint"
	}
	if argv0 == "" {
		return nil, distrust("the image has no entrypoint or command to check; pass --exec PATH or --binary FILE")
	}
	rep.Program = argv0

	c.checkUser()
	c.checkProgram(argv0, opt.binary != "")
	if shellCmd {
		c.add("script", "skip", "the entrypoint runs a shell command (-c), and the programs in it aren't checked; pass --exec with a program's path to check it")
	}
	if c.fatal != nil {
		return nil, c.fatal
	}
	return rep, nil
}

// ---------- user ----------

type passwdEntry struct {
	uid, gid int
	name     string
}

func (c *checker) passwd() (map[string]passwdEntry, bool) {
	b, err := c.img.fs.readFile("/etc/passwd")
	if err != nil {
		return nil, false
	}
	out := map[string]passwdEntry{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		uid, e1 := strconv.Atoi(f[2])
		gid, e2 := strconv.Atoi(f[3])
		if e1 == nil && e2 == nil {
			out[f[0]] = passwdEntry{uid, gid, f[0]}
		}
	}
	return out, true
}

func (c *checker) groups() (map[string]int, map[int][]string, bool) {
	b, err := c.img.fs.readFile("/etc/group")
	if err != nil {
		return nil, nil, false
	}
	byName, members := map[string]int{}, map[int][]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 {
			continue
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		byName[f[0]] = gid
		if f[3] != "" {
			members[gid] = strings.Split(f[3], ",")
		}
	}
	return byName, members, true
}

func (c *checker) checkUser() {
	c.uid, c.gid, c.supp, c.userKnown = 0, 0, nil, true
	u := c.user
	if u == "" {
		c.add("user", "ok", "runs as root (the image sets no user)")
		return
	}
	name, group, hasGroup := strings.Cut(u, ":")
	pw, havePW := c.passwd()
	var uname string
	if uid, err := strconv.Atoi(name); err == nil {
		c.uid = uid
		c.gid = 0
		for _, e := range pw {
			if e.uid == uid {
				c.gid, uname = e.gid, e.name
				break
			}
		}
	} else {
		e, ok := pw[name]
		if !ok {
			why := "/etc/passwd has no entry for it"
			if !havePW {
				why = "the image has no /etc/passwd"
			}
			c.add("user", "error", fmt.Sprintf("user %q can't be found: %s", name, why),
				`the runtime fails before starting the program: "unable to find user `+name+`: no matching entries in passwd file"`,
				"use a numeric user such as 65532:65532, or add the user to /etc/passwd")
			c.userKnown = false
			return
		}
		c.uid, c.gid, uname = e.uid, e.gid, name
	}
	byName, members, haveGR := c.groups()
	if hasGroup {
		if gid, err := strconv.Atoi(group); err == nil {
			c.gid = gid
		} else if gid, ok := byName[group]; ok {
			c.gid = gid
		} else {
			why := "/etc/group has no entry for it"
			if !haveGR {
				why = "the image has no /etc/group"
			}
			c.add("user", "error", fmt.Sprintf("group %q can't be found: %s", group, why),
				`the runtime fails before starting the program: "unable to find group `+group+`"`)
			c.userKnown = false
			return
		}
	}
	if uname != "" {
		for gid, ms := range members {
			for _, m := range ms {
				if m == uname {
					c.supp = append(c.supp, gid)
				}
			}
		}
	}
	c.add("user", "ok", fmt.Sprintf("runs as %s (uid %d, gid %d)", u, c.uid, c.gid))
}

func (c *checker) canExec(n *fnode) bool {
	if !c.userKnown {
		return n.mode&0o111 != 0
	}
	if c.uid == 0 || !n.ownerKnown {
		return n.mode&0o111 != 0
	}
	if n.uid == c.uid {
		return n.mode&0o100 != 0
	}
	inGroup := n.gid == c.gid
	for _, g := range c.supp {
		if g == n.gid {
			inGroup = true
		}
	}
	if inGroup {
		return n.mode&0o010 != 0
	}
	return n.mode&0o001 != 0
}

// ---------- program ----------

func (c *checker) lookPath(argv0 string) (string, bool) {
	if strings.Contains(argv0, "/") {
		if !strings.HasPrefix(argv0, "/") {
			wd := "/"
			if c.img.cfg != nil && c.img.cfg.Config.WorkingDir != "" {
				wd = c.img.cfg.Config.WorkingDir
			}
			return path.Join(wd, argv0), true
		}
		return argv0, true
	}
	for _, dir := range strings.Split(c.env["PATH"], ":") {
		if dir == "" {
			continue
		}
		p := path.Join(dir, argv0)
		if _, n, err := c.img.fs.resolve(p); err == nil && n.kind == 'f' {
			return p, true
		}
	}
	return "", false
}

func (c *checker) checkProgram(argv0 string, fromHost bool) {
	p, ok := c.lookPath(argv0)
	if !ok {
		c.add("file", "error", fmt.Sprintf("%q is not in any PATH directory of the image", argv0),
			`the runtime fails with: exec: "`+argv0+`": executable file not found in $PATH`,
			"PATH is "+c.env["PATH"])
		return
	}
	rp, n, err := c.img.fs.resolve(p)
	if err != nil {
		details := []string{`the runtime fails with: exec: "` + p + `": stat ` + p + `: ` + err.Error()}
		if hits := c.findByName(path.Base(p)); len(hits) > 0 {
			details = append(details, "a file with that name is at "+strings.Join(hits, ", "))
		}
		c.add("file", "error", fmt.Sprintf("%s is not in the image (%v)", p, err), details...)
		return
	}
	if n.kind != 'f' {
		c.add("file", "error", fmt.Sprintf("%s is a directory, not a program", p), "exec fails with permission denied")
		return
	}
	where := p
	if rp != p {
		where = p + " -> " + rp
	}
	c.rep.Program = where
	if !c.canExec(n) {
		c.add("permission", "error", fmt.Sprintf("%s is not executable for the image user (mode %04o, owner %d:%d)", rp, n.mode, n.uid, n.gid),
			`the runtime fails with: exec `+p+`: permission denied`,
			"add chmod +x, or COPY --chmod=0755 in the Dockerfile")
	} else {
		c.add("permission", "ok", fmt.Sprintf("%s is executable (mode %04o)", rp, n.mode))
	}
	b, err := n.content()
	if err != nil {
		c.fatal = err
		return
	}
	c.checkFile(rp, b, fromHost, 0)
}

func (c *checker) findByName(base string) []string {
	var hits []string
	for k, v := range c.img.fs.nodes {
		if v.kind != 'd' && path.Base(k) == base {
			hits = append(hits, k)
		}
	}
	sort.Strings(hits)
	if len(hits) > 5 {
		hits = hits[:5]
	}
	return hits
}

func (c *checker) checkFile(p string, b []byte, fromHost bool, depth int) {
	switch {
	case len(b) == 0:
		c.fatal = distrust("%s is empty, so there is nothing to check (a failed build or copy?)", p)
		return
	case bytes.HasPrefix(b, []byte("#!")):
		c.checkScript(p, b, depth)
		return
	case bytes.HasPrefix(b, []byte("\x7fELF")):
	case bytes.HasPrefix(b, []byte("MZ")):
		c.add("format", "error", p+" is a Windows PE program, which Linux can't run", "exec fails with: exec format error", "build with GOOS=linux")
		return
	case len(b) >= 4 && (binary.LittleEndian.Uint32(b) == 0xfeedfacf || binary.LittleEndian.Uint32(b) == 0xfeedface || binary.BigEndian.Uint32(b) == 0xcafebabe):
		c.add("format", "error", p+" is a macOS Mach-O program, which Linux can't run", "exec fails with: exec format error", "build with GOOS=linux")
		return
	default:
		c.fatal = distrust("%s is neither an ELF program nor a script (it starts with %q): it may be damaged, or the output of a failed build", p, preview(b))
		return
	}
	o, err := parseELF(b, p)
	if err != nil {
		c.fatal = err
		return
	}
	if o.typ != elf.ET_EXEC && o.typ != elf.ET_DYN {
		c.add("format", "error", fmt.Sprintf("%s is an ELF %v file, not a program", p, o.typ), "exec fails with: exec format error")
		return
	}
	g := c.readGo(p, b, o)
	if c.fatal != nil {
		return
	}
	if c.checkArch(p, o) {
		c.checkLoader(p, o, g)
	}
	c.checkGo(p, o, g)
}

func preview(b []byte) string {
	if len(b) > 24 {
		b = b[:24]
	}
	return string(b)
}

func (c *checker) checkScript(p string, b []byte, depth int) {
	line := b
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 256 {
		line = line[:256]
	}
	s := strings.TrimLeft(string(line[2:]), " \t")
	interp, _, _ := strings.Cut(s, " ")
	interp, _, _ = strings.Cut(interp, "\t")
	if strings.HasSuffix(interp, "\r") {
		c.add("script", "error", fmt.Sprintf("%s has Windows line endings: the kernel looks for an interpreter named %q", p, interp),
			"exec fails with: no such file or directory",
			"convert the script to LF line endings, for example with dos2unix or a .gitattributes rule")
		return
	}
	if depth > 0 {
		c.add("script", "error", fmt.Sprintf("the interpreter %s is itself a script, which Linux doesn't follow", p), "exec fails with: exec format error")
		return
	}
	if interp == "" {
		c.add("script", "error", p+" starts with #! but names no interpreter", "exec fails with: exec format error")
		return
	}
	c.add("script", "ok", fmt.Sprintf("%s is a script run by %s", p, interp))
	c.add("script", "skip", "the program the script starts isn't checked; pass --exec with its path to check it")
	rp, n, err := c.img.fs.resolve(interp)
	if err != nil {
		c.add("script", "error", fmt.Sprintf("the script interpreter %s is not in the image (%v)", interp, err),
			"exec fails with: exec "+p+": no such file or directory")
		return
	}
	if n.kind != 'f' {
		c.add("script", "error", "the script interpreter "+interp+" is not a file")
		return
	}
	if !c.canExec(n) {
		c.add("permission", "error", fmt.Sprintf("the script interpreter %s is not executable for the image user (mode %04o)", rp, n.mode))
	}
	data, err := n.content()
	if err != nil {
		c.fatal = err
		return
	}
	c.checkFile(rp, data, false, depth+1)
}

// checkArch reports whether the loader check should go on.
func (c *checker) checkArch(p string, o *elfObj) bool {
	got := o.arch()
	if c.arch == "" {
		c.add("architecture", "skip", fmt.Sprintf("%s is %s; the image architecture is unknown (pass --platform)", p, got))
		return true
	}
	want := c.arch
	if got == want {
		c.add("architecture", "ok", fmt.Sprintf("%s is %s, the same as the image", p, got))
		return true
	}
	if (want == "amd64" && got == "386") || (want == "arm64" && got == "arm") {
		c.add("architecture", "warn", fmt.Sprintf("%s is %s but the image is %s", p, got, want),
			"this runs only on nodes whose kernel supports 32-bit programs")
		return true
	}
	c.add("architecture", "error", fmt.Sprintf("%s is %s but the image is %s", p, got, want),
		"exec fails with: exec format error",
		fmt.Sprintf("build with GOARCH=%s, or build the image for %s", want, got),
		"the loader and library checks were skipped, because the kernel stops here")
	return false
}

// ---------- loader and libraries ----------

func (c *checker) checkLoader(p string, o *elfObj, g *goInfo) {
	if o.interp == "" {
		if o.dynamic && len(o.needed) > 0 {
			c.add("loader", "error", p+" needs shared libraries but names no loader", "exec fails or crashes at start")
			return
		}
		c.add("loader", "ok", p+" is statically linked and needs no loader or shared libraries")
		return
	}
	base := path.Base(o.interp)
	switch {
	case strings.HasPrefix(base, "ld-musl"):
		c.libc = "musl"
	default:
		c.libc = "glibc"
	}
	rp, n, err := c.img.fs.resolve(o.interp)
	if err != nil {
		details := []string{"exec fails with: exec " + p + ": no such file or directory (the message means the loader, not the program)"}
		if c.libc == "glibc" && c.hasMusl() {
			details = append(details, "the image uses musl (Alpine) and the program was linked against glibc")
		}
		if c.libc == "musl" && c.hasGlibc() {
			details = append(details, "the image uses glibc and the program was linked against musl")
		}
		switch {
		case g != nil && g.CGO != "1" && g.BuildMode == "pie":
			details = append(details, "this is a -buildmode=pie build, which asks for a loader even with CGO_ENABLED=0; build without -buildmode=pie, or use a base image with a C library such as distroless/base")
		case g != nil && g.CGO == "1":
			details = append(details, "it was built with CGO_ENABLED=1; build with CGO_ENABLED=0 for a static binary, or use a base image with the same C library as the build image")
		case g != nil && strings.Contains(g.ldflags, "linkmode") && strings.Contains(g.ldflags, "external"):
			details = append(details, "it was linked by the external linker; add -extldflags=-static, or use a base image with the same C library as the build image")
		default:
			details = append(details, "use a base image with the same C library as the build, or link the program statically")
		}
		c.add("loader", "error", fmt.Sprintf("%s needs the loader %s, which is not in the image", p, o.interp), details...)
		return
	}
	if n.kind != 'f' {
		c.add("loader", "error", fmt.Sprintf("the loader %s is not a file", o.interp))
		return
	}
	ld, err := n.content()
	if err != nil {
		c.fatal = err
		return
	}
	lo, err := parseELF(ld, rp)
	if err != nil {
		c.fatal = err
		return
	}
	if lo.machine != o.machine || lo.class != o.class {
		c.add("loader", "error", fmt.Sprintf("the loader %s is %s, but %s is %s", rp, lo.arch(), p, o.arch()), "exec fails with: exec format error")
		return
	}
	c.add("loader", "ok", fmt.Sprintf("loader %s is in the image (%s)", o.interp, c.libc))
	c.checkLibs(p, o)
}

func (c *checker) hasMusl() bool {
	for k := range c.img.fs.nodes {
		if strings.HasPrefix(k, "/lib/ld-musl-") {
			return true
		}
	}
	return false
}

func (c *checker) hasGlibc() bool {
	for k := range c.img.fs.nodes {
		if strings.HasSuffix(k, "/libc.so.6") {
			return true
		}
	}
	return false
}

// ldCache parses /etc/ld.so.cache (the glibc-ld.so.cache1.1 format).
func (c *checker) ldCache() map[string]string {
	if c.cached {
		return c.cache
	}
	c.cached = true
	b, err := c.img.fs.readFile("/etc/ld.so.cache")
	if err != nil {
		return nil
	}
	magic := []byte("glibc-ld.so.cache1.1")
	start := bytes.Index(b, magic)
	if start < 0 || start+48 > len(b) {
		c.add("library", "warn", "/etc/ld.so.cache is in a format wontexec doesn't read; only the default directories were searched")
		return nil
	}
	h := b[start:]
	bo := binary.ByteOrder(binary.LittleEndian)
	n := bo.Uint32(h[20:])
	if int(n) > len(h)/24 {
		bo = binary.BigEndian
		n = bo.Uint32(h[20:])
	}
	out := map[string]string{}
	for i := 0; i < int(n); i++ {
		e := 48 + i*24
		if e+24 > len(h) {
			c.add("library", "warn", "/etc/ld.so.cache is cut off; only the default directories were searched")
			return nil
		}
		k, ok1 := cstr(h, bo.Uint32(h[e+4:]))
		v, ok2 := cstr(h, bo.Uint32(h[e+8:]))
		if ok1 && ok2 {
			if _, dup := out[k]; !dup {
				out[k] = v
			}
			// Several entries for other architectures may share a name; keep all.
			out[k+"\x00"+v] = v
		}
	}
	c.cache = out
	return out
}

func (c *checker) defaultDirs(o *elfObj) []string {
	if c.libc == "musl" {
		march := strings.TrimSuffix(strings.TrimPrefix(path.Base(o.interp), "ld-musl-"), ".so.1")
		if b, err := c.img.fs.readFile("/etc/ld-musl-" + march + ".path"); err == nil {
			var dirs []string
			for _, f := range strings.FieldsFunc(string(b), func(r rune) bool { return r == ':' || r == '\n' }) {
				if f = strings.TrimSpace(f); f != "" {
					dirs = append(dirs, f)
				}
			}
			return dirs
		}
		return []string{"/lib", "/usr/local/lib", "/usr/lib"}
	}
	var dirs []string
	if t, ok := triplets[o.arch()]; ok {
		dirs = append(dirs, "/lib/"+t, "/usr/lib/"+t)
	}
	if o.class == elf.ELFCLASS64 {
		dirs = append(dirs, "/lib64", "/usr/lib64")
	}
	return append(dirs, "/lib", "/usr/lib")
}

func expandOrigin(dirs []string, from string) []string {
	var out []string
	for _, d := range dirs {
		d = strings.ReplaceAll(d, "${ORIGIN}", path.Dir(from))
		d = strings.ReplaceAll(d, "$ORIGIN", path.Dir(from))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

type lib struct {
	path string // the path the loader opens, before symlinks
	obj  *elfObj
}

// findLib searches for a library the way the loader does, and skips files
// for another architecture as the loader does.
func (c *checker) findLib(name string, from *elfObj, main *elfObj) (*lib, []string, error) {
	var order []string
	add := func(dirs ...string) { order = append(order, dirs...) }
	if strings.Contains(name, "/") {
		add(path.Dir(name))
		name = path.Base(name)
	} else {
		if c.libc == "glibc" && len(from.runpath) == 0 {
			add(expandOrigin(from.rpath, from.path)...)
			if from != main && len(main.runpath) == 0 {
				add(expandOrigin(main.rpath, main.path)...)
			}
		}
		if v := c.env["LD_LIBRARY_PATH"]; v != "" {
			add(strings.Split(v, ":")...)
		}
		if c.libc == "musl" {
			add(expandOrigin(from.rpath, from.path)...)
		}
		add(expandOrigin(from.runpath, from.path)...)
	}
	var cands []string
	for _, d := range order {
		if d != "" {
			cands = append(cands, path.Join(d, name))
		}
	}
	if c.libc == "glibc" && !strings.Contains(name, "/") {
		cache := c.ldCache()
		var cached []string
		for k, v := range cache {
			if strings.HasPrefix(k, name+"\x00") {
				cached = append(cached, v)
			}
		}
		sort.Strings(cached)
		cands = append(cands, cached...)
	}
	for _, d := range c.defaultDirs(main) {
		cands = append(cands, path.Join(d, name))
	}
	var wrong []string
	for _, cand := range cands {
		rp, n, err := c.img.fs.resolve(cand)
		if err != nil || n.kind != 'f' {
			continue
		}
		b, err := n.content()
		if err != nil {
			return nil, nil, err
		}
		if !bytes.HasPrefix(b, []byte("\x7fELF")) {
			wrong = append(wrong, rp+" (not ELF)")
			continue
		}
		lo, err := parseELF(b, rp)
		if err != nil {
			return nil, nil, err
		}
		if lo.machine != main.machine || lo.class != main.class {
			wrong = append(wrong, rp+" ("+lo.arch()+")")
			continue
		}
		return &lib{path: cand, obj: lo}, nil, nil
	}
	return nil, wrong, nil
}

func (c *checker) checkLibs(p string, main *elfObj) {
	queue := []*elfObj{main}
	found := map[string]*lib{}
	missing := 0
	total := 0
	for len(queue) > 0 && len(found) < 500 {
		o := queue[0]
		queue = queue[1:]
		libs := map[string]*lib{}
		for _, name := range o.needed {
			if l, ok := found[name]; ok {
				libs[name] = l
				continue
			}
			if c.seen["missing:"+name] {
				continue
			}
			l, wrong, err := c.findLib(name, o, main)
			if err != nil {
				c.fatal = err
				return
			}
			if l == nil {
				c.seen["missing:"+name] = true
				missing++
				msg := fmt.Sprintf("%s needs %s, which the loader can't find in the image", o.path, name)
				var details []string
				if c.libc == "musl" {
					details = append(details, "the loader fails with: Error loading shared library "+name+": No such file or directory")
				} else {
					details = append(details, "the loader fails with: error while loading shared libraries: "+name+": cannot open shared object file: No such file or directory")
				}
				if len(wrong) > 0 {
					details = append(details, "found only for another architecture: "+strings.Join(wrong, ", "))
				} else if hits := c.findByName(name); len(hits) > 0 {
					details = append(details, "a file with that name is at "+strings.Join(hits, ", ")+", which isn't on the search path; set LD_LIBRARY_PATH or run ldconfig in the image")
				}
				c.add("library", "error", msg, details...)
				continue
			}
			found[name] = l
			libs[name] = l
			total++
			queue = append(queue, l.obj)
		}
		c.checkVersions(o, libs)
	}
	if missing == 0 {
		names := make([]string, 0, len(found))
		for n := range found {
			names = append(names, n)
		}
		sort.Strings(names)
		if total == 0 {
			c.add("library", "ok", "it needs no shared libraries besides the loader")
		} else if total == 1 {
			c.add("library", "ok", "its one shared library was found: "+names[0])
		} else {
			c.add("library", "ok", fmt.Sprintf("all %d shared libraries were found: %s", total, strings.Join(names, ", ")))
		}
	}
}

func (c *checker) checkVersions(o *elfObj, libs map[string]*lib) {
	for _, vn := range o.verneed {
		l, ok := libs[vn.file]
		if !ok {
			continue
		}
		var missing []string
		for _, v := range vn.versions {
			if !l.obj.verdef[v] && !vn.weak[v] {
				missing = append(missing, v)
			}
		}
		key := o.path + "\x00" + vn.file
		if c.seen[key] {
			continue
		}
		c.seen[key] = true
		if len(missing) > 0 && !l.obj.hasVer {
			c.add("symbol-version", "warn", fmt.Sprintf("%s has no symbol version data, but %s asks for versions of it (%s)", l.path, o.path, strings.Join(missing, ", ")),
				"glibc prints \"no version information available\" and goes on, which may crash later")
			continue
		}
		if len(missing) > 0 {
			details := []string{"the loader fails with:"}
			for _, v := range missing {
				details = append(details, fmt.Sprintf("  %s: %s: version `%s' not found (required by %s)", o.path, l.path, v, o.path))
			}
			sorted := append([]string{}, missing...)
			sortVersions(sorted)
			missing = sorted
			have := newestVersion(l.obj.verdef, missing[0])
			if have != "" {
				details = append(details, "the newest matching version in the image is "+have)
			}
			details = append(details, "build against an older C library (an older build image), use a newer base image, or build with CGO_ENABLED=0")
			c.add("symbol-version", "error", fmt.Sprintf("%s needs %s from %s, which the image's copy doesn't have", o.path, strings.Join(missing, ", "), vn.file), details...)
		}
	}
}

// sortVersions orders names such as GLIBC_2.17 and GLIBC_2.34 by number.
func sortVersions(v []string) {
	sort.Slice(v, func(i, j int) bool { return versionLess(v[i], v[j]) })
}

func versionLess(a, b string) bool {
	pa, na := splitVersion(a)
	pb, nb := splitVersion(b)
	if pa != pb {
		return pa < pb
	}
	for i := 0; i < len(na) && i < len(nb); i++ {
		if na[i] != nb[i] {
			return na[i] < nb[i]
		}
	}
	return len(na) < len(nb)
}

func splitVersion(s string) (string, []int) {
	prefix, rest, ok := strings.Cut(s, "_")
	if !ok {
		return s, nil
	}
	var nums []int
	for _, f := range strings.Split(rest, ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return s, nil
		}
		nums = append(nums, n)
	}
	return prefix, nums
}

func newestVersion(defs map[string]bool, like string) string {
	prefix, _ := splitVersion(like)
	var best string
	for d := range defs {
		p, nums := splitVersion(d)
		if p != prefix || nums == nil {
			continue
		}
		if best == "" || versionLess(best, d) {
			best = d
		}
	}
	return best
}

// ---------- Go runtime files ----------

var certFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}
var certDirs = []string{"/etc/ssl/certs", "/etc/pki/tls/certs"}
var zoneDirs = []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo"}

var pclnMagics = [][]byte{
	{0xf1, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xf1}, // Go 1.20+
	{0xf0, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xf0}, // Go 1.18
	{0xfa, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xfa}, // Go 1.16
	{0xfb, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xfb}, // Go 1.2
}

func (g *goInfo) has(fn string) bool {
	return bytes.Contains(g.pcln, []byte("\x00"+fn+"\x00"))
}

func (g *goInfo) hasPrefix(pkg string) bool {
	return bytes.Contains(g.pcln, []byte("\x00"+pkg))
}

// readGo returns build info for a Go program, or nil for anything else.
func (c *checker) readGo(p string, b []byte, o *elfObj) *goInfo {
	if _, ok := o.sections[".go.buildinfo"]; !ok {
		if _, ok := o.sections[".gopclntab"]; !ok {
			return nil
		}
	}
	bi, err := buildinfo.Read(bytes.NewReader(b))
	if err != nil {
		c.fatal = distrust("%s looks like a Go program but its build info can't be read: %v (the file may be damaged)", p, err)
		return nil
	}
	g := &goInfo{Version: bi.GoVersion, Module: bi.Path}
	for _, s := range bi.Settings {
		switch s.Key {
		case "CGO_ENABLED":
			g.CGO = s.Value
		case "GOOS":
			g.GOOS = s.Value
		case "GOARCH":
			g.GOARCH = s.Value
		case "-buildmode":
			g.BuildMode = s.Value
		case "-ldflags":
			g.ldflags = s.Value
		}
	}
	for _, d := range bi.Deps {
		if d.Path == "golang.org/x/crypto/x509roots/fallback" {
			g.fallback = true
		}
	}
	c.rep.Go = g
	return g
}

func (c *checker) checkGo(p string, o *elfObj, g *goInfo) {
	if g == nil {
		c.add("go", "skip", p+" is not a Go program, so the CA certificate and time zone checks don't apply")
		return
	}
	if g.GOOS != "" && g.GOOS != "linux" && g.GOOS != "android" {
		c.add("go", "error", fmt.Sprintf("%s was built for GOOS=%s", p, g.GOOS), "build with GOOS=linux")
	}
	pcln, ok := o.sections[".gopclntab"]
	known := false
	for _, m := range pclnMagics {
		if bytes.HasPrefix(pcln, m) {
			known = true
		}
	}
	if !ok || !known {
		c.add("go", "warn", p+": Go's function table can't be read, so the CA certificate and time zone checks were not run")
		return
	}
	g.pcln = pcln
	c.checkCerts(p, g)
	c.checkZones(p, g)
}

func (c *checker) checkCerts(p string, g *goInfo) {
	if !g.has("crypto/x509.loadSystemRoots") {
		c.add("ca-certificates", "ok", p+" doesn't load system CA certificates")
		return
	}
	if g.fallback || g.hasPrefix("golang.org/x/crypto/x509roots/fallback.") {
		c.add("ca-certificates", "ok", p+" embeds fallback CA roots (golang.org/x/crypto/x509roots/fallback)")
		return
	}
	var checked []string
	for _, k := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := c.env[k]; ok {
			for _, f := range strings.Split(v, ":") {
				if f == "" {
					continue
				}
				checked = append(checked, f)
				if c.pemFile(f) || c.img.fs.dirHasFiles(f) {
					c.add("ca-certificates", "ok", fmt.Sprintf("%s=%s points at CA certificates", k, v))
					return
				}
			}
			c.add("ca-certificates", "warn", fmt.Sprintf("%s=%s is set but holds no certificates in the image", k, v),
				"Go reads only this location when it is set, so TLS connections fail with: x509: certificate signed by unknown authority")
			return
		}
	}
	for _, f := range certFiles {
		if c.pemFile(f) {
			c.add("ca-certificates", "ok", "CA certificates found at "+f)
			return
		}
	}
	for _, d := range certDirs {
		if c.img.fs.dirHasFiles(d) {
			c.add("ca-certificates", "ok", "CA certificates found in "+d)
			return
		}
	}
	c.add("ca-certificates", "warn", p+" loads the system CA certificates, and the image has none",
		"outgoing TLS connections fail with: tls: failed to verify certificate: x509: certificate signed by unknown authority",
		"copy /etc/ssl/certs/ca-certificates.crt from the build stage, use a base image with certificates such as distroless/static, or import golang.org/x/crypto/x509roots/fallback")
}

func (c *checker) pemFile(p string) bool {
	_, n, err := c.img.fs.resolve(p)
	if err != nil || n.kind != 'f' || n.size == 0 && !n.loaded {
		return false
	}
	b, err := n.content()
	if err != nil {
		return false
	}
	return bytes.Contains(b, []byte("-----BEGIN CERTIFICATE-----"))
}

func (c *checker) checkZones(p string, g *goInfo) {
	embedded := g.hasPrefix("time/tzdata.")
	tz, hasTZ := c.env["TZ"]
	tz = strings.TrimPrefix(tz, ":")
	if hasTZ && tz != "" && tz != "UTC" && !strings.HasPrefix(tz, "/") && !embedded {
		if v, ok := c.env["ZONEINFO"]; ok && c.img.fs.exists(v) {
			c.add("time-zones", "ok", fmt.Sprintf("TZ=%s, with ZONEINFO=%s", tz, v))
			return
		}
		found := false
		for _, d := range zoneDirs {
			if _, n, err := c.img.fs.resolve(path.Join(d, tz)); err == nil && n.kind == 'f' {
				found = true
			}
		}
		if !found {
			c.add("time-zones", "warn", fmt.Sprintf("the image sets TZ=%s but has no zone file for it, and %s doesn't embed time/tzdata", tz, p),
				"Go silently uses UTC for local time",
				"install tzdata in the image, or build with -tags timetzdata or import _ \"time/tzdata\"")
			return
		}
	}
	if !g.has("time.LoadLocation") {
		c.add("time-zones", "ok", p+" doesn't call time.LoadLocation")
		return
	}
	if embedded {
		c.add("time-zones", "ok", p+" embeds time zone data (time/tzdata)")
		return
	}
	if v, ok := c.env["ZONEINFO"]; ok && c.img.fs.exists(v) {
		c.add("time-zones", "ok", "ZONEINFO="+v+" is in the image")
		return
	}
	for _, d := range zoneDirs {
		if c.img.fs.dirHasFiles(d) {
			c.add("time-zones", "ok", "time zone data found in "+d)
			return
		}
	}
	c.add("time-zones", "warn", p+" calls time.LoadLocation, and the image has no time zone data",
		`loading a named zone fails with: unknown time zone Europe/Berlin (any name other than "UTC" and "Local")`,
		"install tzdata in the image, or build with -tags timetzdata or import _ \"time/tzdata\"")
}

// ---------- output ----------

func printText(w io.Writer, r *report) {
	fmt.Fprintf(w, "image:   %s (%s", r.Image, r.Format)
	if r.Platform != "" {
		fmt.Fprintf(w, ", %s", r.Platform)
	}
	fmt.Fprintf(w, ", %d layer%s)\n", r.Layers, plural(r.Layers))
	fmt.Fprintf(w, "program: %s (%s)", r.Program, r.Source)
	if r.Go != nil {
		fmt.Fprint(w, ", Go program")
		if r.Go.CGO != "" {
			fmt.Fprintf(w, ", CGO_ENABLED=%s", r.Go.CGO)
		}
	}
	fmt.Fprintln(w)
	for _, f := range r.Findings {
		label := map[string]string{"ok": "ok   ", "skip": "skip ", "warn": "WARN ", "error": "ERROR"}[f.Level]
		fmt.Fprintf(w, "  %s %-15s %s\n", label, f.Check, f.Message)
		for _, d := range f.Details {
			fmt.Fprintf(w, "        %-15s %s\n", "", d)
		}
	}
	switch {
	case r.Errors > 0:
		fmt.Fprintf(w, "  %d error%s, %d warning%s: this program won't start in this image as it is.\n", r.Errors, plural(r.Errors), r.Warnings, plural(r.Warnings))
	case r.Warnings > 0:
		fmt.Fprintf(w, "  0 errors, %d warning%s: it should start, but the warnings above can make it fail later.\n", r.Warnings, plural(r.Warnings))
	default:
		msg := "  No problems found in the checks above."
		if r.Skipped > 0 {
			msg += fmt.Sprintf(" %d check%s skipped.", r.Skipped, plural(r.Skipped))
		}
		fmt.Fprintln(w, msg)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const usage = `wontexec checks whether a Linux program will start inside a container image.

usage: wontexec [flags] IMAGE

IMAGE is a docker save tar, an OCI layout (tar or directory), a root
filesystem tar (.tar or .tar.gz), or a root filesystem directory.

flags:
  --binary FILE     check a program from this machine as if copied into the image
  --at PATH         where --binary goes in the image (default: the entrypoint path)
  --exec PATH       check this program in the image instead of the entrypoint
  --platform P      os/arch[/variant] to pick from a multi-platform image
  --user U          check as this user (uid[:gid] or name), as runAsUser would
  --format F        text (default) or json
  --fail-on L       lowest level that exits 1: warn (default), error or none
  --version         print the version

Exit 0: no findings at the --fail-on level. Exit 1: findings.
Exit 2: bad usage, or an input that can't be read fully. Exit 2 ignores --fail-on.
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	opt := options{format: "text", failOn: "warn"}
	var pos []string
	vals := map[string]*string{"--binary": &opt.binary, "--at": &opt.at, "--exec": &opt.exec, "--platform": &opt.platform,
		"--user": &opt.user, "--format": &opt.format, "--fail-on": &opt.failOn}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprint(stdout, usage)
			return 0
		}
		if a == "--version" || a == "-version" {
			fmt.Fprintln(stdout, "wontexec", version)
			return 0
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			name, val, hasVal := strings.Cut(a, "=")
			if !strings.HasPrefix(name, "--") {
				name = "-" + name
			}
			dst, ok := vals[name]
			if !ok {
				fmt.Fprintf(stderr, "wontexec: unknown flag %s\n\n%s", a, usage)
				return 2
			}
			if !hasVal {
				if i+1 >= len(args) {
					fmt.Fprintf(stderr, "wontexec: %s needs a value\n", name)
					return 2
				}
				i++
				val = args[i]
			}
			*dst = val
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "wontexec: give exactly one IMAGE\n\n%s", usage)
		return 2
	}
	if opt.format != "text" && opt.format != "json" {
		fmt.Fprintln(stderr, "wontexec: --format must be text or json")
		return 2
	}
	if opt.failOn != "warn" && opt.failOn != "error" && opt.failOn != "none" {
		fmt.Fprintln(stderr, "wontexec: --fail-on must be warn, error or none")
		return 2
	}
	if opt.at != "" && opt.binary == "" {
		fmt.Fprintln(stderr, "wontexec: --at needs --binary")
		return 2
	}
	if opt.platform != "" && len(strings.Split(opt.platform, "/")) < 2 {
		fmt.Fprintln(stderr, "wontexec: --platform must look like linux/amd64")
		return 2
	}
	rep, err := run(pos[0], opt)
	if err != nil {
		fmt.Fprintf(stderr, "wontexec: can't check: %v\n", err)
		fmt.Fprintln(stderr, "wontexec: no result. This is not a pass.")
		if opt.format == "json" {
			json.NewEncoder(stdout).Encode(map[string]any{"error": err.Error(), "result": "unknown"})
		}
		return 2
	}
	if opt.format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else {
		printText(stdout, rep)
	}
	switch {
	case opt.failOn == "none":
		return 0
	case rep.Errors > 0:
		return 1
	case opt.failOn == "warn" && rep.Warnings > 0:
		return 1
	}
	return 0
}
