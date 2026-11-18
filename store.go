// The forge's whole state: one in-memory store for git objects,
// repositories, issues, pull requests, comments and labels. Every surface
// reads and writes this one store, so a change made over any surface is
// visible on all the others immediately.
//
// Objects are stored as raw bytes keyed by SHA-1, exactly as pushed; a clone
// re-sends the same bytes, so client-built commits round-trip byte-exactly.
// Commits this server builds use a fixed identity and a counter timestamp,
// so their bytes are deterministic.
package main

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"time"
)

type objectType string

const (
	objCommit objectType = "commit"
	objTree   objectType = "tree"
	objBlob   objectType = "blob"
	objTag    objectType = "tag"
)

type gitObject struct {
	typ  objectType
	data []byte
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID        int
	GlobalID  string
	Body      string
	CreatedAt string
}

type Issue struct {
	Number        int
	GlobalID      string
	Title         string
	Body          string
	State         string // "open" | "closed"
	Labels        []*Label
	Comments      []*Comment
	Seq           int
	CreatedAt     string
	UpdatedAt     string
	jsonCache     string
	jsonCacheHost string
}

type PullRequest struct {
	Number         int
	GlobalID       string
	Title          string
	Body           string
	State          string // "open" | "closed"
	Head           string
	Base           string
	HeadSHA        string
	BaseSHA        string
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	Seq            int
	CreatedAt      string
	UpdatedAt      string
}

type Repo struct {
	Owner         string
	Name          string
	DBID          int
	IsPrivate     bool
	Description   string
	DefaultBranch string
	Refs          map[string]string
	Issues        map[int]*Issue
	Pulls         map[int]*PullRequest
	Labels        map[string]*Label
	NextNumber    int
	NextCommentID int
	CreatedAt     string
	UpdatedAt     string
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

const commitEpoch = 1700000000
const commitIdentity = "adaiversen <adaiversen@users.noreply.github.com>"

func repoKey(owner, name string) string { return owner + "/" + name }

func repoGlobalID(owner, name string) string {
	return "R_" + b64urlEncode(owner+"/"+name)
}

func issueGlobalID(owner, name string, number int) string {
	return "I_" + b64urlEncode(owner+"/"+name+"#"+itoa(number))
}

func pullGlobalID(owner, name string, number int) string {
	return "PR_" + b64urlEncode(owner+"/"+name+"#"+itoa(number))
}

func commentGlobalID(owner, name string, number, commentID int) string {
	return "C_" + b64urlEncode(owner+"/"+name+"#"+itoa(number)+":"+itoa(commentID))
}

func userGlobalID(login string) string { return "U_" + b64urlEncode(login) }

type decodedID struct {
	Kind      string
	Owner     string
	Name      string
	Number    int
	CommentID int
}

// decodeGlobalID decodes a global id back to its parts, or nil.
func decodeGlobalID(id string) *decodedID {
	i := strings.IndexByte(id, '_')
	if i < 0 {
		return nil
	}
	prefix := id[:i]
	rest, ok := b64urlDecode(id[i+1:])
	if !ok {
		return nil
	}
	switch prefix {
	case "R":
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			return nil
		}
		return &decodedID{Kind: "R", Owner: parts[0], Name: parts[1]}
	case "I", "PR":
		h := strings.IndexByte(rest, '#')
		if h < 0 {
			return nil
		}
		parts := strings.SplitN(rest[:h], "/", 2)
		if len(parts) != 2 {
			return nil
		}
		n, ok := atoiStrict(rest[h+1:])
		if !ok {
			return nil
		}
		return &decodedID{Kind: prefix, Owner: parts[0], Name: parts[1], Number: n}
	case "C":
		h := strings.IndexByte(rest, '#')
		c := strings.IndexByte(rest, ':')
		if h < 0 || c < 0 {
			return nil
		}
		parts := strings.SplitN(rest[:h], "/", 2)
		if len(parts) != 2 {
			return nil
		}
		n, ok1 := atoiStrict(rest[h+1 : c])
		cid, ok2 := atoiStrict(rest[c+1:])
		if !ok1 || !ok2 {
			return nil
		}
		return &decodedID{Kind: "C", Owner: parts[0], Name: parts[1], Number: n, CommentID: cid}
	}
	return nil
}

func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			// allow a leading minus for robustness (never produced by us)
			if i == 0 && c == '-' {
				continue
			}
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if strings.HasPrefix(s, "-") {
		return -n, true
	}
	return n, true
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// compareTreeNames is git's tree-entry sort: byte order of the name, a
// directory as if it ended in '/'.
func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hexDecode(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func hexDecode(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexVal(s[2*i])
		lo, ok2 := hexVal(s[2*i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		out[i] = hi<<4 | lo
	}
	return out, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		space := bytes.IndexByte(data[off:], ' ')
		if space < 0 {
			break
		}
		space += off
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:space]),
			Name: string(data[space+1 : nul]),
			OID:  hexEncode(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = digits[v>>4]
		out[2*i+1] = digits[v&0xf]
	}
	return string(out)
}

func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var view CommitView
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		key := line
		val := ""
		if sp >= 0 {
			key, val = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			view.Tree = val
		case "parent":
			view.Parents = append(view.Parents, val)
		case "author":
			view.Author = val
		case "committer":
			view.Committer = val
		}
	}
	if blank >= 0 {
		view.Message = string(data[blank+2:])
	}
	return view
}

func parseTag(data []byte) (object, typ, tag, message string) {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		switch line[:sp] {
		case "object":
			object = line[sp+1:]
		case "type":
			typ = line[sp+1:]
		case "tag":
			tag = line[sp+1:]
		}
	}
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	return
}

// lineDiff counts whole-line additions/deletions by longest common subsequence.
func lineDiff(oldText, newText string) (additions, deletions int) {
	var a, b []string
	if oldText != "" {
		a = strings.Split(oldText, "\n")
	}
	if newText != "" {
		b = strings.Split(newText, "\n")
	}
	n, m := len(a), len(b)
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
		for j := range cur {
			cur[j] = 0
		}
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}

var typeCode = map[objectType]int{
	objCommit: 1, objTree: 2, objBlob: 3, objTag: 4,
}
var codeType = map[int]objectType{
	1: objCommit, 2: objTree, 3: objBlob, 4: objTag,
}

type Store struct {
	mu              sync.Mutex
	objects         map[string]*gitObject
	repos           map[string]*Repo
	nextRepoID      int
	commitSeq       int
	issueSeq        int
	depth1PackCache map[string][]byte
	packCache       map[string][]byte
}

func NewStore() *Store {
	return &Store{
		objects:         make(map[string]*gitObject),
		repos:           make(map[string]*Repo),
		nextRepoID:      1,
		depth1PackCache: make(map[string][]byte),
		packCache:       make(map[string][]byte),
	}
}

func (s *Store) putObject(typ objectType, data []byte, oidOpt ...string) string {
	oid := ""
	if len(oidOpt) > 0 && oidOpt[0] != "" {
		oid = oidOpt[0]
	} else {
		oid = objectID(string(typ), data)
	}
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = &gitObject{typ: typ, data: data}
	}
	return oid
}

func (s *Store) getObject(oid string) *gitObject { return s.objects[oid] }

func (s *Store) createRepo(owner, name string, isPrivate bool, description string) *Repo {
	now := nowISO()
	repo := &Repo{
		Owner: owner, Name: name, DBID: s.nextRepoID, IsPrivate: isPrivate,
		Description: description, DefaultBranch: "main",
		Refs: make(map[string]string), Issues: make(map[int]*Issue),
		Pulls: make(map[int]*PullRequest), Labels: make(map[string]*Label),
		NextNumber: 1, NextCommentID: 1, CreatedAt: now, UpdatedAt: now,
	}
	s.nextRepoID++
	s.repos[repoKey(owner, name)] = repo
	return repo
}

func (s *Store) getRepo(owner, name string) *Repo { return s.repos[repoKey(owner, name)] }

func (s *Store) deleteRepo(owner, name string) bool {
	_, ok := s.repos[repoKey(owner, name)]
	delete(s.repos, repoKey(owner, name))
	return ok
}

func (s *Store) listRepos(owner string) []*Repo {
	var out []*Repo
	for _, r := range s.repos {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	return out
}

// buildCommit builds a server-made commit object with a deterministic identity.
func (s *Store) buildCommit(tree string, parents []string, message string) (oid string, data []byte) {
	ts := commitEpoch + s.commitSeq
	s.commitSeq++
	who := commitIdentity + " " + itoa(ts) + " +0000"
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + who + "\n")
	b.WriteString("committer " + who + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	data = []byte(b.String())
	return s.putObject(objCommit, data), data
}

// flattenTree flattens a tree into path -> {oid, mode}.
func (s *Store) flattenTree(treeOID string, prefix string, out map[string]TreeEntry) map[string]TreeEntry {
	if out == nil {
		out = make(map[string]TreeEntry)
	}
	obj := s.getObject(treeOID)
	if obj == nil || obj.typ != objTree {
		return out
	}
	for _, e := range parseTree(obj.data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			s.flattenTree(e.OID, path, out)
		} else {
			out[path] = e
		}
	}
	return out
}

// buildTreeFromPaths builds the tree for a flat path map, bottom-up.
func (s *Store) buildTreeFromPaths(paths map[string]TreeEntry) string {
	dirs := map[string]map[string]TreeEntry{"": {}}
	for path, entry := range paths {
		slash := strings.LastIndexByte(path, '/')
		dir, name := "", path
		if slash >= 0 {
			dir, name = path[:slash], path[slash+1:]
		}
		if _, ok := dirs[dir]; !ok {
			dirs[dir] = make(map[string]TreeEntry)
		}
		dirs[dir][name] = entry
	}
	depthOf := func(dir string) int {
		if dir == "" {
			return 0
		}
		return strings.Count(dir, "/") + 1
	}
	names := make([]string, 0, len(dirs))
	for d := range dirs {
		names = append(names, d)
	}
	sort.Slice(names, func(i, j int) bool { return depthOf(names[i]) > depthOf(names[j]) })
	treeOIDs := make(map[string]string)
	for _, dir := range names {
		var entries []TreeEntry
		for name, e := range dirs[dir] {
			entries = append(entries, TreeEntry{Mode: e.Mode, Name: name, OID: e.OID})
		}
		for childDir, childOID := range treeOIDs {
			slash := strings.LastIndexByte(childDir, '/')
			parent, name := "", childDir
			if slash >= 0 {
				parent, name = childDir[:slash], childDir[slash+1:]
			}
			if parent != dir {
				continue
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: childOID})
		}
		data := buildTree(entries)
		treeOIDs[dir] = s.putObject(objTree, data)
	}
	return treeOIDs[""]
}

// collectReachable returns all objects reachable from roots.
func (s *Store) collectReachable(roots []string, maxDepth int) map[string]bool {
	seen := make(map[string]bool)
	type item struct {
		oid   string
		depth int
	}
	var queue []item
	for _, r := range roots {
		if isSHA1(r) {
			queue = append(queue, item{r, 1})
		}
	}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[cur.oid] {
			continue
		}
		obj := s.objects[cur.oid]
		if obj == nil {
			continue
		}
		seen[cur.oid] = true
		if maxDepth > 0 && cur.depth >= maxDepth {
			continue
		}
		switch obj.typ {
		case objCommit:
			view := parseCommit(obj.data)
			queue = append(queue, item{view.Tree, cur.depth + 1})
			for _, p := range view.Parents {
				queue = append(queue, item{p, cur.depth + 1})
			}
		case objTag:
			target, _, _, _ := parseTag(obj.data)
			queue = append(queue, item{target, cur.depth + 1})
		case objTree:
			for _, e := range parseTree(obj.data) {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, item{e.OID, cur.depth + 1})
			}
		}
	}
	return seen
}

// mergeBase returns the first common ancestor of two commits (BFS from b
// over a's ancestor set), or "".
func (s *Store) mergeBase(a, b string) string {
	ancestors := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if ancestors[oid] {
			continue
		}
		ancestors[oid] = true
		if obj := s.objects[oid]; obj != nil && obj.typ == objCommit {
			queue = append(queue, parseCommit(obj.data).Parents...)
		}
	}
	seenB := make(map[string]bool)
	queueB := []string{b}
	for len(queueB) > 0 {
		oid := queueB[0]
		queueB = queueB[1:]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if ancestors[oid] {
			return oid
		}
		if obj := s.objects[oid]; obj != nil && obj.typ == objCommit {
			queueB = append(queueB, parseCommit(obj.data).Parents...)
		}
	}
	return ""
}

// buildPack builds a packfile of plain (non-delta) objects, in the given
// order. Identical object lists build identical packs; only packs up to
// 64 KiB are kept, at most 64 entries (then cleared).
func (s *Store) buildPack(oids []string) []byte {
	key := strings.Join(oids, ",")
	if cached, ok := s.packCache[key]; ok {
		return cached
	}
	var body bytes.Buffer
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2})
	body.Write([]byte{byte(len(oids) >> 24), byte(len(oids) >> 16), byte(len(oids) >> 8), byte(len(oids))})
	for _, oid := range oids {
		obj := s.objects[oid]
		if obj == nil {
			continue
		}
		body.Write(encodeVarint(typeCode[obj.typ], len(obj.data)))
		body.Write(zlibDeflate(obj.data))
	}
	pack := make([]byte, 0, body.Len()+20)
	pack = append(pack, body.Bytes()...)
	sum := sha1Hex(body.Bytes())
	raw, _ := hexDecode(sum)
	pack = append(pack, raw...)
	if len(pack) <= 64*1024 {
		if len(s.packCache) >= 64 {
			s.packCache = make(map[string][]byte)
		}
		s.packCache[key] = pack
	}
	return pack
}

// buildEmptyPack is a pack with zero objects.
func (s *Store) buildEmptyPack() []byte {
	header := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	sum, _ := hexDecode(sha1Hex(header))
	return append(append([]byte{}, header...), sum...)
}

// depth1Pack returns the depth-1 pack for a tip: the tip commit, its tree
// and blobs (cached per tip).
func (s *Store) depth1Pack(tip string) []byte {
	if cached, ok := s.depth1PackCache[tip]; ok {
		return cached
	}
	var oids []string
	if commit := s.objects[tip]; commit != nil && commit.typ == objCommit {
		view := parseCommit(commit.data)
		oids = append(oids, tip, view.Tree)
		for _, e := range s.flattenTree(view.Tree, "", nil) {
			oids = append(oids, e.OID)
		}
	}
	pack := s.buildPack(oids)
	s.depth1PackCache[tip] = pack
	return pack
}

type packEntry struct {
	typ  objectType
	data []byte
	oid  string
}

// readPack reads a packfile the client sent. Plain entries only; a delta
// entry is an error.
func (s *Store) readPack(pack []byte) ([]packEntry, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errPack("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, errPack("unsupported pack version")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	out := make([]packEntry, 0, count)
	offset := 12
	limit := len(pack) - 20
	for i := 0; i < count; i++ {
		if offset >= limit {
			return nil, errPack("pack is truncated")
		}
		b := pack[offset]
		typeNum := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		offset++
		for b&0x80 != 0 {
			if offset >= limit {
				return nil, errPack("pack entry size is truncated")
			}
			b = pack[offset]
			offset++
			size += int(b&0x7f) << shift
			shift += 7
		}
		if typeNum == 6 || typeNum == 7 {
			return nil, errPack("delta pack entries are not accepted")
		}
		typ, ok := codeType[int(typeNum)]
		if !ok {
			return nil, errPack("unknown pack entry type")
		}
		data, consumed, err := zlibInflateWithLength(pack[offset:limit])
		if err != nil {
			return nil, errPack("pack entry inflate failed")
		}
		offset += consumed
		if len(data) != size {
			return nil, errPack("pack entry size mismatch")
		}
		out = append(out, packEntry{typ: typ, data: data, oid: objectID(string(typ), data)})
	}
	return out, nil
}

type packError struct{ msg string }

func (e *packError) Error() string { return e.msg }
func errPack(msg string) error     { return &packError{msg} }

// commitFile commits one file on a branch.
func (s *Store) commitFile(repo *Repo, branch, path string, content []byte, message string) (commitSHA, contentSHA string) {
	ref := "refs/heads/" + branch
	tip, ok := repo.Refs[ref]
	blobOID := s.putObject(objBlob, content)
	var paths map[string]TreeEntry
	if !ok {
		paths = make(map[string]TreeEntry)
	} else {
		paths = s.flattenTree(parseCommit(s.objects[tip].data).Tree, "", nil)
	}
	paths[path] = TreeEntry{OID: blobOID, Mode: "100644"}
	treeOID := s.buildTreeFromPaths(paths)
	var parents []string
	if ok {
		parents = []string{tip}
	}
	oid, _ := s.buildCommit(treeOID, parents, message)
	repo.Refs[ref] = oid
	repo.UpdatedAt = nowISO()
	return oid, blobOID
}

// deleteFile deletes one file on a branch; "" when absent.
func (s *Store) deleteFile(repo *Repo, branch, path, message string) string {
	ref := "refs/heads/" + branch
	tip, ok := repo.Refs[ref]
	if !ok {
		return ""
	}
	paths := s.flattenTree(parseCommit(s.objects[tip].data).Tree, "", nil)
	if _, ok := paths[path]; !ok {
		return ""
	}
	delete(paths, path)
	treeOID := s.buildTreeFromPaths(paths)
	oid, _ := s.buildCommit(treeOID, []string{tip}, message)
	repo.Refs[ref] = oid
	repo.UpdatedAt = nowISO()
	return oid
}
