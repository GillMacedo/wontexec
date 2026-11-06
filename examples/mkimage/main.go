// Command mkimage writes a small OCI image layout tar for the wontexec demos.
// The files it adds are placeholders, not real certificates or zone data.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	out := flag.String("o", "", "output tar")
	arch := flag.String("arch", "amd64", "image architecture")
	entry := flag.String("entrypoint", "/app", "entrypoint path")
	user := flag.String("user", "", "image user")
	files := flag.String("files", "", "comma-separated: certs, tzdata, passwd")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "mkimage: -o is required")
		os.Exit(2)
	}

	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for _, f := range strings.Split(*files, ",") {
		switch f {
		case "certs":
			add("etc/ssl/certs/ca-certificates.crt", "-----BEGIN CERTIFICATE-----\nplaceholder for the demo, not a real certificate\n-----END CERTIFICATE-----\n")
		case "tzdata":
			add("usr/share/zoneinfo/UTC", "TZif placeholder for the demo\n")
			add("usr/share/zoneinfo/Europe/Lisbon", "TZif placeholder for the demo\n")
		case "passwd":
			add("etc/passwd", "root:x:0:0:root:/root:/sbin/nologin\nnonroot:x:65532:65532:nonroot:/home/nonroot:/sbin/nologin\n")
			add("etc/group", "root:x:0:\nnonroot:x:65532:\n")
		case "":
		default:
			fmt.Fprintln(os.Stderr, "mkimage: unknown file set", f)
			os.Exit(2)
		}
	}
	tw.Close()
	diff := sha256.Sum256(layer.Bytes())
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(layer.Bytes())
	zw.Close()

	digest := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	cfg := map[string]any{
		"architecture": *arch,
		"os":           "linux",
		"config":       map[string]any{"Entrypoint": []string{*entry}, "User": *user, "Env": []string{"PATH=/usr/local/bin:/usr/bin:/bin"}},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + hex.EncodeToString(diff[:])}},
	}
	cb, _ := json.Marshal(cfg)
	man := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(cb), "size": len(cb)},
		"layers":        []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": digest(gz.Bytes()), "size": gz.Len()}},
	}
	mb, _ := json.Marshal(man)
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests":     []map[string]any{{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest(mb), "size": len(mb)}},
	})

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
	ow := tar.NewWriter(f)
	put := func(name string, b []byte) {
		ow.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		ow.Write(b)
	}
	put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	put("index.json", idx)
	for _, b := range [][]byte{cb, mb, gz.Bytes()} {
		put("blobs/sha256/"+strings.TrimPrefix(digest(b), "sha256:"), b)
	}
	if err := ow.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		os.Exit(1)
	}
}
