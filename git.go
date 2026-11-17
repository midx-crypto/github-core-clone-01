// git.go: git objects (SHA-1, zlib packs, pkt-line), the upload-pack v2 +
// v0 and receive-pack v0 smart-HTTP handlers, and the Store's git helpers
// (contents commits, trees, ancestry, diffs) that the REST handlers share.
//
// Protocol notes (from program/env/src/git-client.ts, the suite's client):
//   - info/refs with Git-Protocol: version=2 answers a v2 capability
//     advertisement ("version 2" first line); without it, a v0 ref
//     advertisement opening with "# service=git-upload-pack" /
//     "# service=git-receive-pack" and a flush line.
//   - v2 fetch: command=fetch, sections shallow-info / packfile separated
//     by delim, pack bytes inside sideband band 1. The acknowledgments
//     section is sent only when the request carried haves (git 2.43's
//     fetch reader expects the packfile section first on a haveless fetch).
//   - v0 fetch: NAK/ACK lines, then shallow lines, then sideband pack.
//   - receive-pack is v0 only, advertises report-status + side-band-64k +
//     delete-refs; the report-status stream rides inside sideband band 1.
//   - every update must fast-forward; a non-fast-forward ref is refused
//     per-ref with "ng <ref> non-fast-forward" and never moves.
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------------------
// object ids and codecs
// ---------------------------------------------------------------------------

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree decodes a tree object's entries in stored order.
func ParseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[off : off+sp])
		rest := data[off+sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+21 {
			break
		}
		name := string(rest[:nul])
		oid := hex.EncodeToString(rest[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = off + sp + 1 + nul + 21
	}
	return out
}

// treeSortKey is git's tree sort order: directory names sort as name + "/".
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// BuildTreeBytes encodes entries sorted in git tree order.
func BuildTreeBytes(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		raw, err := hex.DecodeString(e.OID)
		if err != nil || len(raw) != 20 {
			continue
		}
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ParseCommit decodes a commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func ParseCommit(data []byte) CommitView {
	var cv CommitView
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
		cv.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(head), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := line[:sp], line[sp+1:]
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		case "committer":
			cv.Committer = val
		}
	}
	return cv
}

// BuildCommitBytes encodes a commit object. The message is forced to end
// with exactly one newline, matching the suite client's buildCommit.
func BuildCommitBytes(tree string, parents []string, author, committer, message string) []byte {
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// ParseTag decodes an annotated tag object.
type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func ParseTag(data []byte) TagView {
	var tv TagView
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
		tv.Message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(head), "\n") {
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		switch line[:sp] {
		case "object":
			tv.Object = line[sp+1:]
		case "type":
			tv.Type = line[sp+1:]
		case "tag":
			tv.Tag = line[sp+1:]
		case "tagger":
			tv.Tagger = line[sp+1:]
		}
	}
	return tv
}

// ident builds a "Name <email> <ts> +0000" line from a login.
func ident(login string) string {
	return login + " <" + login + "@example.invalid> 1700000000 +0000"
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

// pktLine encodes one pkt-line (payload already includes its trailing \n).
func pktLine(payload []byte) []byte {
	out := make([]byte, 4, len(payload)+4)
	hex.Encode(out[:4], []byte{byte((len(payload) + 4) >> 8), byte(len(payload) + 4)})
	// hex-encode the length by hand: 4 ascii hex digits.
	n := len(payload) + 4
	out[0] = "0123456789abcdef"[n>>12&0xf]
	out[1] = "0123456789abcdef"[n>>8&0xf]
	out[2] = "0123456789abcdef"[n>>4&0xf]
	out[3] = "0123456789abcdef"[n&0xf]
	return append(out, payload...)
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// pktLines encodes a list of lines, where "flush"/"delim" are control words.
func pktLines(lines []string) []byte {
	var out []byte
	for _, l := range lines {
		switch l {
		case "flush":
			out = append(out, pktFlush()...)
		case "delim":
			out = append(out, pktDelim()...)
		default:
			out = append(out, pktLine([]byte(l))...)
		}
	}
	return out
}

// pktReader walks pkt-lines over a byte slice.
type pktReader struct {
	buf []byte
	pos int
}

func (p *pktReader) next() (kind string, payload []byte, ok bool) {
	if p.pos+4 > len(p.buf) {
		return "", nil, false
	}
	head := string(p.buf[p.pos : p.pos+4])
	if head == "0000" {
		p.pos += 4
		return "flush", nil, true
	}
	if head == "0001" {
		p.pos += 4
		return "delim", nil, true
	}
	n, err := strconv.ParseUint(head, 16, 32)
	if err != nil || n < 4 || p.pos+int(n) > len(p.buf) {
		return "", nil, false
	}
	payload = p.buf[p.pos+4 : p.pos+int(n)]
	p.pos += int(n)
	return "data", payload, true
}

// rest returns the unread remainder (the pack after a flush).
func (p *pktReader) rest() []byte { return p.buf[p.pos:] }

// ---------------------------------------------------------------------------
// packfiles (plain objects only on write; deltas resolved on read)
// ---------------------------------------------------------------------------

var packTypeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packCodeType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func writePackObjectHeader(typ string, size int) []byte {
	code := packTypeCode[typ]
	var out []byte
	first := (code << 4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	return append(out, first)
}

// BuildPack encodes objects as a pack v2 with a SHA-1 trailer. The order is
// the caller's; pass oids sorted for deterministic bytes.
//
// zlib writers are pooled: a fresh zlib.NewWriter per object allocates a
// ~1MB flate window each time, which dominated FetchPack's allocations
// (3.2MB/op) and GC pressure. Reset preserves the DefaultCompression level,
// so pooled output is byte-identical to a fresh writer.
var zlibWriterPool = sync.Pool{New: func() any { return zlib.NewWriter(io.Discard) }}

func BuildPack(objects []GitObject) []byte {
	var body bytes.Buffer
	count := len(objects)
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2, byte(count >> 24), byte(count >> 16), byte(count >> 8), byte(count)})
	for _, o := range objects {
		body.Write(writePackObjectHeader(o.Type, len(o.Data)))
		var zb bytes.Buffer
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&zb)
		zw.Write(o.Data)
		zw.Close()
		body.Write(zb.Bytes())
		zlibWriterPool.Put(zw)
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// applyDelta applies a git delta to a base buffer.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		if off >= len(delta) {
			return 0, errors.New("delta truncated")
		}
		var size int
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			c := delta[off]
			off++
			size |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return size, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size %d, want %d", srcSize, len(base))
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			var cpOff, cpSize int
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					cpOff |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					cpSize |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			if off+int(op) > len(delta) {
				return nil, errors.New("delta truncated")
			}
			out = append(out, delta[off:off+int(op)]...)
			off += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != tgtSize {
		return nil, fmt.Errorf("delta target size %d, want %d", len(out), tgtSize)
	}
	return out, nil
}

func readPackVarint(buf []byte, off int) (size int, first byte, next int, err error) {
	if off >= len(buf) {
		return 0, 0, 0, errors.New("pack truncated")
	}
	b := buf[off]
	first = b
	off++
	size = int(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, 0, errors.New("pack truncated")
		}
		b = buf[off]
		off++
		size |= int(b&0x7f) << shift
		shift += 7
	}
	return size, first, off, nil
}

// readPack parses a packfile, resolving OFS/REF deltas. resolveBase is
// consulted for REF_DELTA bases outside the pack (thin packs); nil is fine.
func readPack(pack []byte, resolveBase func(oid string) (*GitObject, bool)) (map[string]*GitObject, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	sum := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(sum[:], pack[trailerStart:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}
	type raw struct {
		offset   int
		typeCode byte
		size     int
		data     []byte
		baseOff  int
		baseOID  string
		resolved []byte
		resType  string
		oid      string
	}
	raws := make([]*raw, 0, count)
	byOffset := make(map[int]*raw)
	off := 12
	for i := 0; i < count; i++ {
		entryOff := off
		size, first, next, err := readPackVarint(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		typeCode := (first >> 4) & 0x07
		e := &raw{offset: entryOff, typeCode: typeCode, size: size}
		if typeCode == 6 {
			// OFS_DELTA: negative offset varint.
			if off >= len(pack) {
				return nil, errors.New("pack truncated")
			}
			c := pack[off]
			off++
			baseOff := int(c & 0x7f)
			for c&0x80 != 0 {
				if off >= len(pack) {
					return nil, errors.New("pack truncated")
				}
				c = pack[off]
				off++
				baseOff = ((baseOff + 1) << 7) | int(c&0x7f)
			}
			e.baseOff = entryOff - baseOff
		} else if typeCode == 7 {
			if off+20 > trailerStart {
				return nil, errors.New("pack truncated")
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if packCodeType[typeCode] == "" {
			return nil, fmt.Errorf("unknown pack type %d", typeCode)
		}
		zr, err := zlib.NewReader(bytes.NewReader(pack[off:trailerStart]))
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack entry declares %d bytes, inflated %d", size, len(data))
		}
		e.data = data
		// Advance past the zlib stream: inflate the raw deflate stream
		// through a bytes.Reader (an io.ByteReader, so flate reads
		// exactly the stream's bytes, no over-read), then add the 2-byte
		// zlib header and the 4-byte adler32 trailer.
		off += zlibStreamLen(pack[off:trailerStart])
		raws = append(raws, e)
		byOffset[entryOff] = e
	}
	if off != trailerStart {
		return nil, errors.New("pack has stray bytes before its trailer")
	}
	objects := make(map[string]*GitObject, count)
	byOID := make(map[string]*raw)
	resolving := make(map[int]bool)
	var resolve func(e *raw) error
	resolve = func(e *raw) error {
		if e.resolved != nil {
			return nil
		}
		if resolving[e.offset] {
			return errors.New("delta cycle")
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			base, ok := byOffset[e.baseOff]
			if !ok {
				return errors.New("ofs-delta base not in pack")
			}
			if err := resolve(base); err != nil {
				return err
			}
			typ = base.resType
			d, err := applyDelta(base.resolved, e.data)
			if err != nil {
				return err
			}
			data = d
		case 7:
			if base, ok := byOID[e.baseOID]; ok {
				if err := resolve(base); err != nil {
					return err
				}
				typ = base.resType
				d, err := applyDelta(base.resolved, e.data)
				if err != nil {
					return err
				}
				data = d
			} else if resolveBase != nil {
				ext, ok := resolveBase(e.baseOID)
				if !ok {
					return fmt.Errorf("ref-delta base %s not available", e.baseOID)
				}
				typ = ext.Type
				d, err := applyDelta(ext.Data, e.data)
				if err != nil {
					return err
				}
				data = d
			} else {
				return fmt.Errorf("ref-delta base %s not in pack", e.baseOID)
			}
		default:
			typ = packCodeType[e.typeCode]
			data = e.data
		}
		e.resolved = data
		e.resType = typ
		e.oid = hashObject(typ, data)
		byOID[e.oid] = e
		if _, ok := objects[e.oid]; !ok {
			objects[e.oid] = &GitObject{Type: typ, Data: data}
		}
		return nil
	}
	for _, e := range raws {
		if err := resolve(e); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

// zlibStreamLen returns the byte length of the zlib stream at the start of b.
func zlibStreamLen(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	br := bytes.NewReader(b)
	br.Seek(2, io.SeekStart)
	fr := flate.NewReader(br)
	_, _ = io.Copy(io.Discard, fr)
	fr.Close()
	consumedDeflate := len(b) - 2 - br.Len()
	return 2 + consumedDeflate + 4
}

// ---------------------------------------------------------------------------
// Store git objects + refs (contract methods)
// ---------------------------------------------------------------------------

func (s *Store) PutObject(repo *Repo, typ string, data []byte) string {
	sha := hashObject(typ, data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.objects[repo.ID]; ok {
		if _, exists := m[sha]; !exists {
			cp := make([]byte, len(data))
			copy(cp, data)
			m[sha] = &GitObject{Type: typ, Data: cp}
		}
	}
	return sha
}

func (s *Store) GetObject(repo *Repo, sha string) (*GitObject, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objects[repo.ID][sha]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(o.Data))
	copy(cp, o.Data)
	return &GitObject{Type: o.Type, Data: cp}, true
}

// getObjectLocked returns the stored object without copying; caller holds
// at least a read lock and must not mutate the result.
func (s *Store) getObjectLocked(repo *Repo, sha string) (*GitObject, bool) {
	o, ok := s.objects[repo.ID][sha]
	return o, ok
}

func (s *Store) GetRef(repo *Repo, ref string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sha, ok := s.refs[repo.ID][ref]
	return sha, ok
}

func (s *Store) SetRef(repo *Repo, ref, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[repo.ID][ref] = sha
}

func (s *Store) DeleteRef(repo *Repo, ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refs[repo.ID], ref)
}

func (s *Store) ListRefs(repo *Repo) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.refs[repo.ID]))
	for k, v := range s.refs[repo.ID] {
		out[k] = v
	}
	return out
}

// CommitTree creates a commit object; caller holds the write lock in the
// internal path (commitTreeLocked2) or the public method takes it.
func (s *Store) CommitTree(repo *Repo, treeSHA, message, user string, parents []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitTreeLocked2(repo, treeSHA, message, user, parents)
}

// commitTreeLocked2 creates a commit object; caller holds the lock.
func (s *Store) commitTreeLocked2(repo *Repo, treeSHA, message, user string, parents []string) string {
	who := ident(user)
	data := BuildCommitBytes(treeSHA, parents, who, who, message)
	sha := hashObject("commit", data)
	s.objects[repo.ID][sha] = &GitObject{Type: "commit", Data: data}
	return sha
}

// commitTreeLocked returns the tree sha of a commit; "" when missing/not a commit.
func (s *Store) commitTreeLocked(repo *Repo, commitSHA string) string {
	o, ok := s.objects[repo.ID][commitSHA]
	if !ok || o.Type != "commit" {
		return ""
	}
	return ParseCommit(o.Data).Tree
}

// BuildTree builds nested tree objects from path -> blob sha; caller holds
// the lock in the internal path, or the public method takes it.
func (s *Store) BuildTree(repo *Repo, entries map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildTreeLocked(repo, entries)
}

func (s *Store) buildTreeLocked(repo *Repo, entries map[string]string) string {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: map[string]string{}, dirs: map[string]*node{}} }
	root := mk()
	for path, blob := range entries {
		parts := strings.Split(path, "/")
		n := root
		for _, seg := range parts[:len(parts)-1] {
			if n.dirs[seg] == nil {
				n.dirs[seg] = mk()
			}
			n = n.dirs[seg]
		}
		n.files[parts[len(parts)-1]] = blob
	}
	var build func(n *node) string
	build = func(n *node) string {
		var ents []TreeEntry
		for name, blob := range n.files {
			ents = append(ents, TreeEntry{Mode: "100644", Name: name, OID: blob})
		}
		for name, child := range n.dirs {
			ents = append(ents, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		data := BuildTreeBytes(ents)
		sha := hashObject("tree", data)
		s.objects[repo.ID][sha] = &GitObject{Type: "tree", Data: data}
		return sha
	}
	return build(root)
}

// flattenTreeLocked returns path -> blob sha for a tree, recursively.
func (s *Store) flattenTreeLocked(repo *Repo, treeSHA string) map[string]string {
	out := map[string]string{}
	var walk func(treeSHA, prefix string)
	walk = func(treeSHA, prefix string) {
		o, ok := s.objects[repo.ID][treeSHA]
		if !ok || o.Type != "tree" {
			return
		}
		for _, e := range ParseTree(o.Data) {
			path := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, path+"/")
			} else {
				out[path] = e.OID
			}
		}
	}
	if treeSHA != "" {
		walk(treeSHA, "")
	}
	return out
}

// diffLocked diffs two trees (base -> head) as a sorted path list.
func (s *Store) diffLocked(repo *Repo, baseTree, headTree string) []FileChange {
	base := s.flattenTreeLocked(repo, baseTree)
	head := s.flattenTreeLocked(repo, headTree)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []FileChange
	for _, p := range sorted {
		b, inBase := base[p]
		h, inHead := head[p]
		if inBase && inHead && b == h {
			continue
		}
		ch := FileChange{Path: p}
		switch {
		case inBase && !inHead:
			ch.Status = "removed"
			ch.Deletions = countLinesOf(s, repo, b)
		case !inBase && inHead:
			ch.Status = "added"
			ch.Additions = countLinesOf(s, repo, h)
		default:
			ch.Status = "modified"
			ch.Additions = countLinesOf(s, repo, h)
			ch.Deletions = countLinesOf(s, repo, b)
		}
		ch.Changes = ch.Additions + ch.Deletions
		out = append(out, ch)
	}
	return out
}

// DiffFiles is the public diff helper for pull /files.
func (s *Store) DiffFiles(repo *Repo, baseTree, headTree string) []FileChange {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diffLocked(repo, baseTree, headTree)
}

func countLinesOf(s *Store, repo *Repo, blobSHA string) int {
	o, ok := s.objects[repo.ID][blobSHA]
	if !ok || o.Type != "blob" {
		return 0
	}
	if len(o.Data) == 0 {
		return 0
	}
	n := bytes.Count(o.Data, []byte("\n"))
	if o.Data[len(o.Data)-1] != '\n' {
		n++
	}
	return n
}

// IsAncestor reports whether anc is an ancestor of (or equal to) desc.
func (s *Store) IsAncestor(repo *Repo, anc, desc string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isAncestorLocked(repo, anc, desc)
}

func (s *Store) isAncestorLocked(repo *Repo, anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	queue := []string{desc}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o, ok := s.objects[repo.ID][cur]
		if !ok || o.Type != "commit" {
			continue
		}
		for _, p := range ParseCommit(o.Data).Parents {
			if p == anc {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// ListCommits walks the first-parent history from a ref (branch name or
// full ref), newest first. Returns false when the ref does not exist.
func (s *Store) ListCommits(repo *Repo, ref string) ([]CommitInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tip, ok := s.refs[repo.ID][ref]
	if !ok {
		if !strings.HasPrefix(ref, "refs/") {
			tip, ok = s.refs[repo.ID]["refs/heads/"+ref]
		}
		if !ok {
			return nil, false
		}
	}
	var out []CommitInfo
	seen := map[string]bool{}
	cur := tip
	for cur != "" && !seen[cur] {
		seen[cur] = true
		o, ok := s.objects[repo.ID][cur]
		if !ok || o.Type != "commit" {
			break
		}
		cv := ParseCommit(o.Data)
		out = append(out, CommitInfo{
			SHA: cur, Tree: cv.Tree, Parents: cv.Parents,
			Author: cv.Author, Committer: cv.Committer, Message: cv.Message,
		})
		if len(cv.Parents) == 0 {
			break
		}
		cur = cv.Parents[0]
	}
	return out, true
}

// ---------------------------------------------------------------------------
// contents (each PUT/DELETE creates a git commit on a branch)
// ---------------------------------------------------------------------------

func (s *Store) PutContent(owner, repo, path string, content []byte, sha string, user string) (*Content, string, error) {
	return s.PutContentFull(owner, repo, path, content, sha, "", "Update "+path+"\n", user)
}

// PutContentFull is PutContent with an explicit branch ("" = default) and
// commit message. The design-doc PutContent delegates here with defaults;
// the REST handler must use this so the commit message the suite sent is
// the message the commit carries, and branch commits land on that branch.
func (s *Store) PutContentFull(owner, repo, path string, content []byte, sha, branch, message, user string) (*Content, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, "", errRepoNotFound
	}
	if branch == "" {
		branch = r.DefaultBranch
	}
	ref := "refs/heads/" + branch
	tip, hasTip := s.refs[r.ID][ref]
	var entries map[string]string
	if hasTip {
		entries = s.flattenTreeLocked(r, s.commitTreeLocked(r, tip))
	} else {
		entries = map[string]string{}
	}
	if sha != "" {
		cur, exists := entries[path]
		if !exists || cur != sha {
			return nil, "", errStaleSHA
		}
	}
	blobSHA := hashObject("blob", content)
	cp := make([]byte, len(content))
	copy(cp, content)
	s.objects[r.ID][blobSHA] = &GitObject{Type: "blob", Data: cp}
	entries[path] = blobSHA
	treeSHA := s.buildTreeLocked(r, entries)
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitSHA := s.commitTreeLocked2(r, treeSHA, message, user, parents)
	s.refs[r.ID][ref] = commitSHA
	return &Content{Path: path, SHA: blobSHA, CommitSHA: commitSHA, Data: cp}, commitSHA, nil
}

func (s *Store) GetContent(owner, repo, path string) (*Content, bool) {
	return s.GetContentRef(owner, repo, path, "")
}

// GetContentRef reads a file at a ref ("" = default branch).
func (s *Store) GetContentRef(owner, repo, path, ref string) (*Content, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	branch := ref
	if branch == "" {
		branch = r.DefaultBranch
	}
	tip, ok := s.refs[r.ID]["refs/heads/"+branch]
	if !ok {
		// ref may itself be a commit sha (GET ?ref=<sha>).
		if _, ok := s.objects[r.ID][branch]; ok {
			tip = branch
		} else {
			return nil, false
		}
	}
	entries := s.flattenTreeLocked(r, s.commitTreeLocked(r, tip))
	blobSHA, ok := entries[path]
	if !ok {
		return nil, false
	}
	o, ok := s.objects[r.ID][blobSHA]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(o.Data))
	copy(cp, o.Data)
	return &Content{Path: path, SHA: blobSHA, CommitSHA: tip, Data: cp}, true
}

// ListContentDir lists the entries of a directory at a ref ("" = default).
func (s *Store) ListContentDir(owner, repo, dir, ref string) ([]ContentEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	branch := ref
	if branch == "" {
		branch = r.DefaultBranch
	}
	tip, ok := s.refs[r.ID]["refs/heads/"+branch]
	if !ok {
		if _, ok := s.objects[r.ID][branch]; ok {
			tip = branch
		} else {
			return nil, false
		}
	}
	treeSHA := s.commitTreeLocked(r, tip)
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	// Walk to the directory's tree.
	if dir != "" {
		cur := treeSHA
		for _, seg := range strings.Split(dir, "/") {
			o, ok := s.objects[r.ID][cur]
			if !ok || o.Type != "tree" {
				return nil, false
			}
			found := ""
			for _, e := range ParseTree(o.Data) {
				if e.Name == seg && e.Mode == "40000" {
					found = e.OID
					break
				}
			}
			if found == "" {
				return nil, false
			}
			cur = found
		}
		treeSHA = cur
	}
	o, ok := s.objects[r.ID][treeSHA]
	if !ok || o.Type != "tree" {
		return nil, false
	}
	var out []ContentEntry
	for _, e := range ParseTree(o.Data) {
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		out = append(out, ContentEntry{Type: typ, Name: e.Name, Path: prefix + e.Name, SHA: e.OID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true
}

func (s *Store) DeleteContent(owner, repo, path, sha string, user string) (string, error) {
	return s.DeleteContentFull(owner, repo, path, sha, "", "Delete "+path+"\n", user)
}

// DeleteContentFull is DeleteContent with an explicit branch and message.
func (s *Store) DeleteContentFull(owner, repo, path, sha, branch, message, user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return "", errRepoNotFound
	}
	if branch == "" {
		branch = r.DefaultBranch
	}
	ref := "refs/heads/" + branch
	tip, ok := s.refs[r.ID][ref]
	if !ok {
		return "", errNotFound
	}
	entries := s.flattenTreeLocked(r, s.commitTreeLocked(r, tip))
	cur, exists := entries[path]
	if !exists {
		return "", errNotFound
	}
	if sha != "" && cur != sha {
		return "", errStaleSHA
	}
	delete(entries, path)
	treeSHA := s.buildTreeLocked(r, entries)
	commitSHA := s.commitTreeLocked2(r, treeSHA, message, user, []string{tip})
	s.refs[r.ID][ref] = commitSHA
	return commitSHA, nil
}

// ---------------------------------------------------------------------------
// fetch selection
// ---------------------------------------------------------------------------

// fetchSelection is what one upload-pack fetch sends.
type fetchSelection struct {
	objects   []GitObject // sorted by oid
	shallow   []string    // new shallow boundary commits
	unshallow []string    // previous shallow commits now fully included
	acks      []string    // haves the server shares with the wants
	nak       bool
}

// selectFetchLocked computes the fetch; caller holds at least a read lock.
func (s *Store) selectFetchLocked(repo *Repo, wants, haves, shallowBounds []string, depth int, deepenRelative bool) fetchSelection {
	boundary := map[string]bool{}
	for _, b := range shallowBounds {
		boundary[b] = true
	}
	// A relative deepen (git's --deepen) adds to the depth the client
	// already has: the boundary's generation from the want, plus one.
	if depth > 0 && deepenRelative && len(shallowBounds) > 0 {
		gen := map[string]int{}
		type item struct {
			oid string
			g   int
		}
		stack := []item{}
		for _, w := range wants {
			stack = append(stack, item{w, 0})
		}
		maxGen := -1
		for len(stack) > 0 {
			it := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if old, seen := gen[it.oid]; seen && old <= it.g {
				continue
			}
			gen[it.oid] = it.g
			if boundary[it.oid] && it.g > maxGen {
				maxGen = it.g
			}
			o, ok := s.objects[repo.ID][it.oid]
			if !ok || o.Type != "commit" {
				continue
			}
			for _, p := range ParseCommit(o.Data).Parents {
				stack = append(stack, item{p, it.g + 1})
			}
		}
		if maxGen >= 0 {
			depth = depth + maxGen + 1
		}
	}
	// haveSet: everything the client holds. Traverse from haves but never
	// descend past a shallow boundary (its parents are not held).
	haveSet := map[string]bool{}
	var holdWalk func(oid string)
	holdWalk = func(oid string) {
		if haveSet[oid] {
			return
		}
		o, ok := s.objects[repo.ID][oid]
		if !ok {
			return
		}
		haveSet[oid] = true
		switch o.Type {
		case "commit":
			cv := ParseCommit(o.Data)
			holdWalk(cv.Tree)
			if !boundary[oid] {
				for _, p := range cv.Parents {
					holdWalk(p)
				}
			}
		case "tree":
			for _, e := range ParseTree(o.Data) {
				holdWalk(e.OID)
			}
		case "tag":
			tv := ParseTag(o.Data)
			holdWalk(tv.Object)
		}
	}
	for _, h := range haves {
		holdWalk(h)
	}

	// wantClosure: commits within `depth` generations of the wants
	// (depth 1 = the wanted commits only; depth <= 0 = unlimited), plus
	// their full tree closures. Tags peel to their target at the same depth.
	wantSet := map[string]bool{}
	var wantWalk func(oid string, gen int)
	wantWalk = func(oid string, gen int) {
		if wantSet[oid] {
			return
		}
		o, ok := s.objects[repo.ID][oid]
		if !ok {
			return
		}
		wantSet[oid] = true
		switch o.Type {
		case "commit":
			cv := ParseCommit(o.Data)
			wantWalk(cv.Tree, gen)
			if depth <= 0 || gen+1 < depth {
				for _, p := range cv.Parents {
					wantWalk(p, gen+1)
				}
			}
		case "tree":
			for _, e := range ParseTree(o.Data) {
				wantWalk(e.OID, gen)
			}
		case "tag":
			tv := ParseTag(o.Data)
			wantWalk(tv.Object, gen)
		}
	}
	for _, w := range wants {
		wantWalk(w, 0)
	}

	// New objects = wanted but not held.
	var oids []string
	for oid := range wantSet {
		if !haveSet[oid] {
			oids = append(oids, oid)
		}
	}
	sort.Strings(oids)
	objects := make([]GitObject, 0, len(oids))
	for _, oid := range oids {
		if o, ok := s.objects[repo.ID][oid]; ok {
			objects = append(objects, GitObject{Type: o.Type, Data: o.Data})
		}
	}

	// Acks: haves that are ancestors of (or equal to) a want.
	var acks []string
	if len(haves) > 0 {
		ancestors := map[string]bool{}
		var ancWalk func(oid string)
		ancWalk = func(oid string) {
			if ancestors[oid] {
				return
			}
			o, ok := s.objects[repo.ID][oid]
			if !ok {
				return
			}
			ancestors[oid] = true
			if o.Type == "commit" {
				for _, p := range ParseCommit(o.Data).Parents {
					ancWalk(p)
				}
			} else if o.Type == "tag" {
				ancWalk(ParseTag(o.Data).Object)
			}
		}
		for _, w := range wants {
			ancWalk(w)
		}
		for _, h := range haves {
			if ancestors[h] {
				acks = append(acks, h)
			}
		}
	}

	// Shallow boundaries: included commits whose parents were cut by depth.
	var shallow []string
	if depth > 0 {
		for oid := range wantSet {
			o, ok := s.objects[repo.ID][oid]
			if !ok || o.Type != "commit" {
				continue
			}
			parents := ParseCommit(o.Data).Parents
			if len(parents) == 0 {
				continue
			}
			cut := false
			for _, p := range parents {
				if !wantSet[p] {
					cut = true
					break
				}
			}
			if cut {
				shallow = append(shallow, oid)
			}
		}
	}
	// Unshallow: previous boundaries now fully included (parents present).
	var unshallow []string
	for _, b := range shallowBounds {
		o, ok := s.objects[repo.ID][b]
		if !ok || o.Type != "commit" {
			continue
		}
		parents := ParseCommit(o.Data).Parents
		allIn := true
		for _, p := range parents {
			if !wantSet[p] {
				allIn = false
				break
			}
		}
		if allIn && wantSet[b] {
			unshallow = append(unshallow, b)
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return fetchSelection{
		objects: objects, shallow: shallow, unshallow: unshallow,
		acks: acks, nak: len(acks) == 0,
	}
}

// FetchPack builds the pack for one fetch (public; takes the read lock).
// It returns the pack bytes and the shallow-info lines.
func (s *Store) FetchPack(repo *Repo, wants, haves, shallow []string, depth int, deepenRelative bool) (pack []byte, sel fetchSelection) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sel = s.selectFetchLocked(repo, wants, haves, shallow, depth, deepenRelative)
	return BuildPack(sel.objects), sel
}

// ---------------------------------------------------------------------------
// receive-pack application
// ---------------------------------------------------------------------------

// PushCommand is one ref update from a push.
type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

// PushRefResult is one report-status line.
type PushRefResult struct {
	Ref    string
	OK     bool
	Reason string
}

// ApplyPush stores unpacked objects and applies the ref updates, in order.
// It returns the per-ref report. Caller holds no lock.
func (s *Store) ApplyPush(repo *Repo, commands []PushCommand, objects map[string]*GitObject) []PushRefResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for oid, o := range objects {
		if _, ok := s.objects[repo.ID][oid]; !ok {
			s.objects[repo.ID][oid] = o
		}
	}
	results := make([]PushRefResult, 0, len(commands))
	for _, cmd := range commands {
		results = append(results, s.applyCommandLocked(repo, cmd))
	}
	return results
}

func (s *Store) applyCommandLocked(repo *Repo, cmd PushCommand) PushRefResult {
	fail := func(reason string) PushRefResult {
		return PushRefResult{Ref: cmd.Ref, OK: false, Reason: reason}
	}
	cur, exists := s.refs[repo.ID][cmd.Ref]
	if cmd.NewOID == zeroOID {
		// Delete.
		if !exists {
			return fail("ref does not exist")
		}
		if cmd.OldOID != zeroOID && cmd.OldOID != cur {
			return fail("old value does not match")
		}
		delete(s.refs[repo.ID], cmd.Ref)
		return PushRefResult{Ref: cmd.Ref, OK: true}
	}
	if cmd.OldOID == zeroOID {
		// Create.
		if exists {
			return fail("already exists")
		}
	} else {
		if !exists {
			return fail("non-fast-forward")
		}
		if cmd.OldOID != cur {
			return fail("non-fast-forward")
		}
		if cmd.OldOID == cmd.NewOID {
			return PushRefResult{Ref: cmd.Ref, OK: true}
		}
	}
	obj, ok := s.objects[repo.ID][cmd.NewOID]
	if !ok {
		return fail("object not found")
	}
	if obj.Type == "commit" {
		if cmd.OldOID != zeroOID && !s.isAncestorLocked(repo, cmd.OldOID, cmd.NewOID) {
			return fail("non-fast-forward")
		}
	} else {
		// Non-commit targets (tags): only creates are fast-forward-safe.
		if cmd.OldOID != zeroOID {
			return fail("non-fast-forward")
		}
	}
	s.refs[repo.ID][cmd.Ref] = cmd.NewOID
	return PushRefResult{Ref: cmd.Ref, OK: true}
}

// Peel follows annotated tags to the underlying object (public; locks).
func (s *Store) Peel(repo *Repo, oid string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peelLocked(repo, oid)
}

// peelLocked follows annotated tags to the underlying commit (or object).
func (s *Store) peelLocked(repo *Repo, oid string) string {
	for i := 0; i < 8; i++ {
		o, ok := s.objects[repo.ID][oid]
		if !ok || o.Type != "tag" {
			return oid
		}
		oid = ParseTag(o.Data).Object
	}
	return oid
}

// ---------------------------------------------------------------------------
// smart-HTTP handlers
// ---------------------------------------------------------------------------

// RegisterGitRoutes wires the three git endpoints onto the router.
func RegisterGitRoutes(rt *Router, s *Store) {
	rt.Handle("GET", "/{owner}/{repo}.git/info/refs", gitInfoRefs(s))
	rt.Handle("POST", "/{owner}/{repo}.git/git-upload-pack", gitUploadPack(s))
	rt.Handle("POST", "/{owner}/{repo}.git/git-receive-pack", gitReceivePack(s))
}

func lookupGitRepo(s *Store, r *Request) (*Repo, bool) {
	return s.GetRepo(r.Params["owner"], r.Params["repo"])
}

// sortedRefs returns the repo's refs sorted by name.
func sortedRefs(s *Store, repo *Repo) [][2]string {
	m := s.ListRefs(repo)
	var out [][2]string
	for k, v := range m {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// v0Advertisement builds a v0 ref advertisement body.
func v0Advertisement(s *Store, repo *Repo, service, caps string) []byte {
	var body []byte
	body = append(body, pktLine([]byte("# service="+service+"\n"))...)
	body = append(body, pktFlush()...)
	refs := sortedRefs(s, repo)
	first := true
	emit := func(line string) {
		if first {
			line += "\x00" + caps
			first = false
		}
		body = append(body, pktLine([]byte(line+"\n"))...)
	}
	if len(refs) == 0 {
		emit(zeroOID + " capabilities^{}")
		body = append(body, pktFlush()...)
		return body
	}
	if tip, ok := s.GetRef(repo, "refs/heads/"+repo.DefaultBranch); ok {
		emit(tip + " HEAD")
	} else {
		emit(zeroOID + " HEAD")
	}
	for _, kv := range refs {
		emit(kv[1] + " " + kv[0])
		if strings.HasPrefix(kv[0], "refs/tags/") {
			if peeled := s.Peel(repo, kv[1]); peeled != kv[1] {
				body = append(body, pktLine([]byte(peeled+" "+kv[0]+"^{}\n"))...)
			}
		}
	}
	// The advertisement ends with a flush: the reader consumes packets
	// until it sees one, and end-of-body without it is a hung-up remote.
	body = append(body, pktFlush()...)
	return body
}

func gitInfoRefs(s *Store) Handler {
	return func(w *ResponseWriter, r *Request) {
		repo, ok := lookupGitRepo(s, r)
		if !ok {
			JSONError(w, 404, "Not Found")
			return
		}
		service := ""
		for _, kv := range strings.Split(r.RawQuery, "&") {
			if strings.HasPrefix(kv, "service=") {
				service = kv[len("service="):]
			}
		}
		switch service {
		case "git-upload-pack":
			if r.Headers["git-protocol"] == "version=2" {
				w.Header("Content-Type", "application/x-git-upload-pack-advertisement")
				w.Status(200)
				w.Write(pktLines([]string{
					"version 2\n",
					"ls-refs=unborn\n",
					"fetch=shallow\n",
					"object-format=sha1\n",
					"agent=forge-go/1.0\n",
					"flush",
				}))
				return
			}
			w.Header("Content-Type", "application/x-git-upload-pack-advertisement")
			w.Status(200)
			w.Write(v0Advertisement(s, repo, "git-upload-pack",
				"side-band-64k thin-pack ofs-delta shallow multi_ack multi_ack_detailed symref=HEAD:refs/heads/"+repo.DefaultBranch+" agent=forge-go/1.0"))
		case "git-receive-pack":
			w.Header("Content-Type", "application/x-git-receive-pack-advertisement")
			w.Status(200)
			w.Write(v0Advertisement(s, repo, "git-receive-pack",
				"report-status side-band-64k delete-refs ofs-delta agent=forge-go/1.0"))
		default:
			JSONError(w, 400, "unknown service")
		}
	}
}

// sideband wraps pack bytes in band-1 pkt-lines (32 KiB chunks).
func sideband(pack []byte) []byte {
	var out []byte
	for len(pack) > 0 {
		n := len(pack)
		if n > 32768 {
			n = 32768
		}
		chunk := make([]byte, 1, n+1)
		chunk[0] = 1
		chunk = append(chunk, pack[:n]...)
		out = append(out, pktLine(chunk)...)
		pack = pack[n:]
	}
	return out
}

// lsRefsResponse builds the v2 ls-refs answer.
func lsRefsResponse(s *Store, repo *Repo, peel, symrefs, unborn bool, prefixes []string) []byte {
	refs := sortedRefs(s, repo)
	mainTip, hasMain := s.GetRef(repo, "refs/heads/"+repo.DefaultBranch)
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var lines []string
	// HEAD is listed when no prefix filter is given, or when a prefix
	// matches it (git asks "ref-prefix HEAD" on clone); a refs/heads/
	// or refs/tags/ filter alone must not list it.
	headOK := len(prefixes) == 0
	for _, p := range prefixes {
		if strings.HasPrefix("HEAD", p) {
			headOK = true
		}
	}
	if headOK {
		if hasMain {
			head := mainTip + " HEAD"
			if symrefs {
				head += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			lines = append(lines, head+"\n")
		} else if unborn {
			head := "unborn HEAD"
			if symrefs {
				head += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			lines = append(lines, head+"\n")
		}
	}
	for _, kv := range refs {
		if !keep(kv[0]) {
			continue
		}
		line := kv[1] + " " + kv[0]
		if peel && strings.HasPrefix(kv[0], "refs/tags/") {
			if peeled := s.Peel(repo, kv[1]); peeled != kv[1] {
				line += " peeled:" + peeled
			}
		}
		lines = append(lines, line+"\n")
	}
	lines = append(lines, "flush")
	return pktLines(lines)
}

func gitUploadPack(s *Store) Handler {
	return func(w *ResponseWriter, r *Request) {
		repo, ok := lookupGitRepo(s, r)
		if !ok {
			JSONError(w, 404, "Not Found")
			return
		}
		w.Header("Content-Type", "application/x-git-upload-pack-result")
		if r.Headers["git-protocol"] == "version=2" {
			handleUploadPackV2(s, repo, w, r)
			return
		}
		handleUploadPackV0(s, repo, w, r)
	}
}

// parseV2Args collects the data lines of a v2 request body.
func parseV2Args(body []byte) ([]string, bool) {
	pr := &pktReader{buf: body}
	var args []string
	for {
		kind, payload, ok := pr.next()
		if !ok {
			return nil, false
		}
		switch kind {
		case "flush", "delim":
			// sections just separate; keep collecting until the final flush.
			if kind == "flush" {
				return args, true
			}
		case "data":
			args = append(args, string(payload))
		}
	}
}

func handleUploadPackV2(s *Store, repo *Repo, w *ResponseWriter, r *Request) {
	args, ok := parseV2Args(r.Body)
	if !ok || len(args) == 0 {
		JSONError(w, 400, "malformed request")
		return
	}
	command := strings.TrimSuffix(args[0], "\n")
	switch command {
	case "command=ls-refs":
		peel, symrefs, unborn := false, false, false
		var prefixes []string
		for _, a := range args[1:] {
			a = strings.TrimSuffix(a, "\n")
			switch {
			case a == "peel":
				peel = true
			case a == "symrefs":
				symrefs = true
			case a == "unborn":
				unborn = true
			case strings.HasPrefix(a, "ref-prefix "):
				prefixes = append(prefixes, a[len("ref-prefix "):])
			}
		}
		w.Status(200)
		w.Write(lsRefsResponse(s, repo, peel, symrefs, unborn, prefixes))
	case "command=fetch":
		var wants, haves, shallow []string
		depth := 0
		deepenRelative := false
		for _, a := range args[1:] {
			a = strings.TrimSuffix(a, "\n")
			switch {
			case strings.HasPrefix(a, "want "):
				wants = append(wants, a[5:])
			case strings.HasPrefix(a, "have "):
				haves = append(haves, a[5:])
			case strings.HasPrefix(a, "shallow "):
				shallow = append(shallow, a[8:])
			case strings.HasPrefix(a, "deepen "):
				depth, _ = strconv.Atoi(a[7:])
			case a == "deepen-relative":
				deepenRelative = true
			}
		}
		pack, sel := s.FetchPack(repo, wants, haves, shallow, depth, deepenRelative)
		var out []byte
		// The acknowledgments section is sent only when the request
		// carried haves: git 2.43's v2 fetch reader expects the packfile
		// section first on a haveless fetch (it dies with "expected
		// 'packfile', received 'acknowledgments'" otherwise), while the
		// suite client accepts the packfile section first in all cases.
		if len(haves) > 0 {
			out = append(out, pktLine([]byte("acknowledgments\n"))...)
			if sel.nak {
				out = append(out, pktLine([]byte("NAK\n"))...)
			} else {
				for _, ack := range sel.acks {
					out = append(out, pktLine([]byte("ACK "+ack+"\n"))...)
				}
				out = append(out, pktLine([]byte("ready\n"))...)
			}
			out = append(out, pktDelim()...)
		}
		if len(sel.shallow) > 0 || len(sel.unshallow) > 0 {
			out = append(out, pktLine([]byte("shallow-info\n"))...)
			for _, sh := range sel.shallow {
				out = append(out, pktLine([]byte("shallow "+sh+"\n"))...)
			}
			for _, un := range sel.unshallow {
				out = append(out, pktLine([]byte("unshallow "+un+"\n"))...)
			}
			out = append(out, pktDelim()...)
		}
		out = append(out, pktLine([]byte("packfile\n"))...)
		out = append(out, sideband(pack)...)
		out = append(out, pktFlush()...)
		w.Status(200)
		w.Write(out)
	default:
		JSONError(w, 400, "unknown command")
	}
}

func handleUploadPackV0(s *Store, repo *Repo, w *ResponseWriter, r *Request) {
	pr := &pktReader{buf: r.Body}
	var wants, haves, shallow []string
	depth := 0
	deepenRelative := false
	hasDone := false
	section := 0
	for {
		kind, payload, ok := pr.next()
		if !ok {
			break // truncated body: answer with what was parsed
		}
		if kind == "flush" {
			section++
			if section >= 2 {
				break
			}
			continue
		}
		if kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		// The first want line may carry capabilities after a space.
		if strings.HasPrefix(line, "want ") {
			oid := line[5:]
			if sp := strings.IndexByte(oid, ' '); sp >= 0 {
				oid = oid[:sp]
			}
			wants = append(wants, oid)
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, line[5:])
		} else if strings.HasPrefix(line, "shallow ") {
			shallow = append(shallow, line[8:])
		} else if strings.HasPrefix(line, "deepen ") {
			depth, _ = strconv.Atoi(line[7:])
		} else if line == "done" {
			hasDone = true
		}
	}
	// v0 deepen is relative to the current shallow boundary whenever the
	// request carries shallow lines (git sends no deepen-relative in v0);
	// a fresh depth-N clone carries none and is absolute.
	deepenRelative = len(shallow) > 0
	if !hasDone {
		// Probe (git's two-phase shallow fetch): the request is only
		// wants+deepen, and the response must be ONLY the shallow /
		// unshallow lines plus a flush. Sending NAK or a pack here
		// leaves bytes in the stream that poison the second phase's
		// read ("expected shallow list"). The suite client always
		// sends done, so it never takes this branch.
		_, sel := s.FetchPack(repo, wants, haves, shallow, depth, deepenRelative)
		var out []byte
		for _, sh := range sel.shallow {
			out = append(out, pktLine([]byte("shallow "+sh+"\n"))...)
		}
		for _, un := range sel.unshallow {
			out = append(out, pktLine([]byte("unshallow "+un+"\n"))...)
		}
		out = append(out, pktFlush()...)
		w.Status(200)
		w.Write(out)
		return
	}
	pack, sel := s.FetchPack(repo, wants, haves, shallow, depth, deepenRelative)
	var out []byte
	// v0 shallow: the shallow/unshallow lines come FIRST, before the
	// NAK/ACK lines (git dies with "expected shallow/unshallow, got
	// NAK" otherwise). The suite client's v0 parser accepts them in
	// either position.
	for _, sh := range sel.shallow {
		out = append(out, pktLine([]byte("shallow "+sh+"\n"))...)
	}
	for _, un := range sel.unshallow {
		out = append(out, pktLine([]byte("unshallow "+un+"\n"))...)
	}
	// The shallow section is flush-terminated: git reads shallow lines
	// in a loop until a flush, and dies on NAK otherwise. (The suite
	// client's v0 parser skips non-data packets, so this is safe.)
	if len(sel.shallow) > 0 || len(sel.unshallow) > 0 {
		out = append(out, pktFlush()...)
	}
	// multi_ack_detailed ACKs: every common have is "common" except the
	// last, which is "ready" (the server will send the pack now).
	if sel.nak {
		out = append(out, pktLine([]byte("NAK\n"))...)
	} else {
		for i, ack := range sel.acks {
			status := "common"
			if i == len(sel.acks)-1 {
				status = "ready"
			}
			out = append(out, pktLine([]byte("ACK "+ack+" "+status+"\n"))...)
		}
	}
	out = append(out, sideband(pack)...)
	out = append(out, pktFlush()...)
	w.Status(200)
	w.Write(out)
}

func gitReceivePack(s *Store) Handler {
	return func(w *ResponseWriter, r *Request) {
		repo, ok := lookupGitRepo(s, r)
		if !ok {
			JSONError(w, 404, "Not Found")
			return
		}
		pr := &pktReader{buf: r.Body}
		var commands []PushCommand
		for {
			kind, payload, ok := pr.next()
			if !ok {
				JSONError(w, 400, "malformed request")
				return
			}
			if kind == "flush" {
				break
			}
			if kind != "data" {
				continue
			}
			line := strings.TrimSuffix(string(payload), "\n")
			if i := strings.IndexByte(line, 0); i >= 0 {
				line = line[:i] // capabilities after NUL
			}
			parts := strings.SplitN(line, " ", 3)
			if len(parts) != 3 || !isOID(parts[0]) || !isOID(parts[1]) {
				JSONError(w, 400, "malformed command")
				return
			}
			commands = append(commands, PushCommand{OldOID: parts[0], NewOID: parts[1], Ref: parts[2]})
		}
		if len(commands) == 0 {
			JSONError(w, 400, "no commands")
			return
		}
		packBytes := pr.rest()
		var objects map[string]*GitObject
		unpackErr := ""
		if len(packBytes) > 0 {
			parsed, err := readPack(packBytes, func(oid string) (*GitObject, bool) {
				return s.GetObject(repo, oid)
			})
			if err != nil {
				unpackErr = err.Error()
			} else {
				objects = parsed
			}
		}
		w.Header("Content-Type", "application/x-git-receive-pack-result")
		var report []string
		if unpackErr != "" {
			report = append(report, "unpack "+unpackErr+"\n")
			for _, cmd := range commands {
				report = append(report, "ng "+cmd.Ref+" unpack failed\n")
			}
		} else {
			results := s.ApplyPush(repo, commands, objects)
			report = append(report, "unpack ok\n")
			for _, res := range results {
				if res.OK {
					report = append(report, "ok "+res.Ref+"\n")
				} else {
					report = append(report, "ng "+res.Ref+" "+res.Reason+"\n")
				}
			}
		}
		var out []byte
		for _, line := range report {
			chunk := make([]byte, 1, len(line)+1)
			chunk[0] = 1
			chunk = append(chunk, pktLine([]byte(line))...)
			out = append(out, pktLine(chunk)...)
		}
		// The report-status stream itself ends with a flush inside band 1;
		// without it the reader hits end-of-body while waiting for the
		// terminator ("the remote end hung up unexpectedly").
		out = append(out, pktLine([]byte{1, '0', '0', '0', '0'})...)
		out = append(out, pktFlush()...)
		w.Status(200)
		w.Write(out)
	}
}
