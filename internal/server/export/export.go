// Package export is the nightly export's file (decision 040, plan-m9 §2.1): a gzipped tar of the server's tables as
// CSV, with a manifest, encrypted to the escrow's age recipient. Standard tools open it without Holdfast:
//
//	age -d -i key.txt 2026-11-22.age | tar -xz
//
// The credentials inside stay sealed with the server's credentials key, so the export alone opens no repository. Since
// plan-m10 it also holds the agent listener's certificate, key and CA, the UI's certificate and key, and server.yaml
// without the database's DSN, so a rebuilt server shows agents the certificate they pinned. The S3 keys, the database's
// password and the OIDC secret are not in it: the admin gives them again.
package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"time"

	"filippo.io/age"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// Format names this layout; a reader refuses another.
const Format = "holdfast-export-1"

// maxFile bounds one file read back from an export, so a damaged one cannot fill the memory.
const maxFile = 4 << 30

// Manifest is manifest.json, the export's first file.
type Manifest struct {
	Format          string    `json:"format"`
	HoldfastVersion string    `json:"holdfast_version"`
	CreatedAt       time.Time `json:"created_at"`
	Node            string    `json:"node"`
	Schemas         []Schema  `json:"schemas"`
	Files           []File    `json:"files,omitempty"`
}

// File is one of the server's files in the export, named by the server.yaml setting that names its path, such as
// tls.cert_file, so a restore writes it where the new server.yaml says (plan-m10 item 3).
type File struct {
	Name   string `json:"name"`
	File   string `json:"file"` // in the tar: files/<name>
	SHA256 string `json:"sha256"`
}

var fileName = regexp.MustCompile(`^[a-z_.]{1,64}$`)

// Schema is one PostgreSQL schema: public, and offsite with off-site copies (decision 041).
type Schema struct {
	Name    string  `json:"name"`
	Version int     `json:"version"` // the newest migration applied
	Tables  []Table `json:"tables"`
}

// Table is one table's CSV file in the export.
type Table struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

// Contents is an export, opened.
type Contents struct {
	Manifest Manifest
	Tables   map[string][]store.TableData // by schema name
	Files    map[string][]byte            // by File.Name
}

var schemaName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// Seal writes c as a gzipped tar encrypted to the age recipient, filling in the manifest's tables.
func Seal(c Contents, recipient string) ([]byte, error) {
	rcpt, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("escrow.age_recipient: %w", err)
	}
	m := c.Manifest
	m.Format = Format
	files := map[string][]byte{}
	var order []string
	for i := range m.Schemas {
		sc := &m.Schemas[i]
		if !schemaName.MatchString(sc.Name) {
			return nil, fmt.Errorf("schema %q is not a name Holdfast makes", sc.Name)
		}
		sc.Tables = nil
		for _, t := range c.Tables[sc.Name] {
			f := sc.Name + "/" + t.Name + ".csv"
			sum := sha256.Sum256(t.CSV)
			sc.Tables = append(sc.Tables, Table{Name: t.Name, File: f, Rows: t.Rows, SHA256: hex.EncodeToString(sum[:])})
			files[f] = t.CSV
			order = append(order, f)
		}
	}
	m.Files = nil
	names := make([]string, 0, len(c.Files))
	for n := range c.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !fileName.MatchString(n) {
			return nil, fmt.Errorf("file %q is not a name Holdfast writes", n)
		}
		f := "files/" + n
		sum := sha256.Sum256(c.Files[n])
		m.Files = append(m.Files, File{Name: n, File: f, SHA256: hex.EncodeToString(sum[:])})
		files[f] = c.Files[n]
		order = append(order, f)
	}
	man, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc, err := age.Encrypt(&out, rcpt)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: m.CreatedAt, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := put("manifest.json", append(man, '\n')); err != nil {
		return nil, err
	}
	for _, f := range order {
		if err := put(f, files[f]); err != nil {
			return nil, err
		}
	}
	for _, closer := range []io.Closer{tw, gz, enc} {
		if err := closer.Close(); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

// Open decrypts an export with the identity, and checks that it is whole: its format, and each file the manifest
// names, with its checksum.
func Open(data []byte, ids ...age.Identity) (Contents, error) {
	dec, err := age.Decrypt(bytes.NewReader(data), ids...)
	if err != nil {
		return Contents{}, fmt.Errorf("decrypting the export (is it the escrow's age key?): %w", err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return Contents{}, fmt.Errorf("the export is not a gzipped tar: %w", err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Contents{}, fmt.Errorf("reading the export: %w", err)
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || name != h.Name || path.IsAbs(name) || name == ".." || len(name) > 3 && name[:3] == "../" {
			return Contents{}, fmt.Errorf("the export holds %q, which Holdfast does not write", h.Name)
		}
		if _, dup := files[name]; dup {
			return Contents{}, fmt.Errorf("the export holds %s twice", name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxFile+1))
		if err != nil {
			return Contents{}, fmt.Errorf("reading %s from the export: %w", name, err)
		}
		if len(b) > maxFile {
			return Contents{}, fmt.Errorf("%s in the export is larger than %d bytes", name, maxFile)
		}
		files[name] = b
	}
	var m Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		return Contents{}, fmt.Errorf("the export's manifest.json: %w", err)
	}
	if m.Format != Format {
		return Contents{}, fmt.Errorf("the export's format is %q; this Holdfast reads %q", m.Format, Format)
	}
	c := Contents{Manifest: m, Tables: map[string][]store.TableData{}}
	for _, sc := range m.Schemas {
		if !schemaName.MatchString(sc.Name) {
			return Contents{}, fmt.Errorf("the export names schema %q", sc.Name)
		}
		for _, t := range sc.Tables {
			b, ok := files[t.File]
			if !ok || t.File != sc.Name+"/"+t.Name+".csv" {
				return Contents{}, fmt.Errorf("the export lacks %s, which its manifest names", t.File)
			}
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != t.SHA256 {
				return Contents{}, fmt.Errorf("%s in the export does not match its checksum", t.File)
			}
			c.Tables[sc.Name] = append(c.Tables[sc.Name], store.TableData{Name: t.Name, CSV: b, Rows: t.Rows})
		}
	}
	c.Files = map[string][]byte{}
	for _, f := range m.Files {
		b, ok := files[f.File]
		if !fileName.MatchString(f.Name) || f.File != "files/"+f.Name || !ok {
			return Contents{}, fmt.Errorf("the export lacks %s, which its manifest names", f.File)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return Contents{}, fmt.Errorf("%s in the export does not match its checksum", f.File)
		}
		c.Files[f.Name] = b
	}
	return c, nil
}
