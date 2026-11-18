// git smart HTTP: info/refs, git-upload-pack (v2 and v0) and
// git-receive-pack (v0).
//
// Wire shapes follow the suite's own git client exactly:
//   - v2 advertisement: `version 2` + capability lines, no service preamble.
//   - v0 advertisement: `# service=...` + flush + ref lines; the first line
//     carries capabilities after a NUL; an empty repo advertises one
//     `0000...0 capabilities^{}` line; peeled tags follow their tag line.
//   - v2 fetch answers LABELED sections (acknowledgments, shallow-info,
//     wanted-refs, packfile), each closed by a delim packet; empty sections
//     are omitted, and the packfile section always carries a pack (a valid
//     0-object pack when nothing is new).
//   - v0 fetch answers plain NAK/ACK/shallow lines, then the sideband pack.
//   - receive-pack is v0 only; its report-status is the pkt-line stream
//     (`unpack ok`, `ok <ref>` / `ng <ref> <reason>`) inside sideband band 1,
//     and a non-fast-forward update is refused with exactly `non-fast-forward`.
//   - Packs carry plain objects only, no deltas; the trailer is SHA-1.
package main

import (
	"bytes"
	"sort"
	"strings"
)

const uploadAdvertise = "application/x-git-upload-pack-advertisement"
const uploadResult = "application/x-git-upload-pack-result"
const receiveAdvertise = "application/x-git-receive-pack-advertisement"
const receiveResult = "application/x-git-receive-pack-result"

var v2Caps = [][2]string{
	{"agent", agentName},
	{"ls-refs", "unborn"},
	{"fetch", "shallow filter"},
	{"object-format", "sha1"},
}

const v0UploadCaps = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack agent=" + agentName
const v0ReceiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=" + agentName

func sendBuffer(res *Response, status int, body []byte, contentType string) {
	res.WriteHead(status, map[string]string{
		"content-type":   contentType,
		"content-length": itoa(len(body)),
	})
	res.End(body)
}

// sidebandPack frames pack bytes as sideband-64k pkt-lines (band 1), then a flush.
func sidebandPack(pack []byte) []byte {
	var out bytes.Buffer
	const chunk = 65515
	for off := 0; off < len(pack); off += chunk {
		end := off + chunk
		if end > len(pack) {
			end = len(pack)
		}
		frame := make([]byte, 0, 1+end-off)
		frame = append(frame, 1)
		frame = append(frame, pack[off:end]...)
		out.Write(pktLine(frame))
	}
	if len(pack) == 0 {
		out.Write(pktLine([]byte{1}))
	}
	out.Write(pktFlush)
	return out.Bytes()
}

type refEntry struct {
	oid  string
	name string
}

func sortedRefs(repo *Repo) []refEntry {
	refs := make([]refEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, refEntry{oid, name})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func peelOf(s *Store, oid string) (string, bool) {
	current := s.getObject(oid)
	for guard := 0; current != nil && current.typ == objTag && guard < 10; guard++ {
		target, _, _, _ := parseTag(current.data)
		if !isSHA1(target) {
			return "", false
		}
		targetObj := s.getObject(target)
		if targetObj == nil {
			return "", false
		}
		if targetObj.typ == objCommit {
			return target, true
		}
		if targetObj.typ == objTag {
			current = targetObj
			continue
		}
		return "", false
	}
	return "", false
}

func v0Advertisement(s *Store, repo *Repo, service string) []byte {
	caps := v0UploadCaps
	if service == "git-receive-pack" {
		caps = v0ReceiveCaps
	}
	var out bytes.Buffer
	out.Write(pktLineString("# service=" + service + "\n"))
	out.Write(pktFlush)
	refs := sortedRefs(repo)
	mainTip, hasMain := repo.Refs["refs/heads/main"]
	if len(refs) == 0 {
		out.Write(pktLineString(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		first := true
		if hasMain {
			line := mainTip + " HEAD"
			if first {
				line += "\x00" + caps
			}
			out.Write(pktLineString(line + "\n"))
			first = false
		}
		for _, ref := range refs {
			line := ref.oid + " " + ref.name
			if first {
				line += "\x00" + caps
			}
			out.Write(pktLineString(line + "\n"))
			first = false
			if peeled, ok := peelOf(s, ref.oid); ok {
				out.Write(pktLineString(peeled + " " + ref.name + "^{}\n"))
			}
		}
	}
	out.Write(pktFlush)
	return out.Bytes()
}

func v2Advertisement() []byte {
	var out bytes.Buffer
	out.Write(pktLineString("version 2\n"))
	for _, c := range v2Caps {
		out.Write(pktLineString(c[0] + "=" + c[1] + "\n"))
	}
	out.Write(pktFlush)
	return out.Bytes()
}

func isAncestor(s *Store, ancestor, oid string) bool {
	queue := []string{oid}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if obj := s.getObject(cur); obj != nil && obj.typ == objCommit {
			queue = append(queue, parseCommit(obj.data).Parents...)
		}
	}
	return false
}

type fetchPlan struct {
	wants       []string
	haves       []string
	shallow     []string
	depth       int
	hasDepth    bool
	deepenSince int64
	deepenNot   []string
}

// planFetch computes the objects a fetch must send, plus the shallow boundary.
//
// No depth: everything reachable from the wants that the haves do not already
// hold (a tag want contributes the tag object and its target).
//
// With depth: commit generations from the wants (generation 1 = the tips);
// the boundary is the commits at the deepest generation. A shallow clone's
// haves are only the boundary commits and their trees — the client does NOT
// hold their ancestors — so the held set is the haves' own objects, not
// their full ancestry.
func planFetch(s *Store, plan *fetchPlan) (objects []string, shallow, unshallow []string) {
	isShallow := plan.hasDepth || len(plan.shallow) > 0
	held := make(map[string]bool)
	if isShallow {
		for _, oid := range plan.haves {
			obj := s.getObject(oid)
			if obj == nil {
				continue
			}
			held[oid] = true
			if obj.typ == objCommit {
				for oid := range s.collectReachable([]string{parseCommit(obj.data).Tree}, 0) {
					held[oid] = true
				}
			} else if obj.typ == objTag {
				for oid := range s.collectReachable([]string{oid}, 0) {
					held[oid] = true
				}
			}
		}
	} else {
		for oid := range s.collectReachable(plan.haves, 0) {
			held[oid] = true
		}
	}

	if !plan.hasDepth {
		set := make(map[string]bool)
		for oid := range s.collectReachable(plan.wants, 0) {
			if !held[oid] {
				set[oid] = true
			}
		}
		return keysOf(set), nil, nil
	}

	maxGen := plan.depth
	included := make(map[string]int) // oid -> generation
	extra := make(map[string]bool)
	type qitem struct {
		oid string
		gen int
	}
	queue := make([]qitem, 0, len(plan.wants))
	for _, oid := range plan.wants {
		queue = append(queue, qitem{oid, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := included[cur.oid]; ok {
			continue
		}
		obj := s.getObject(cur.oid)
		if obj == nil {
			continue
		}
		if obj.typ == objTag {
			extra[cur.oid] = true
			target, _, _, _ := parseTag(obj.data)
			if isSHA1(target) {
				queue = append(queue, qitem{target, cur.gen})
			}
			continue
		}
		if obj.typ != objCommit {
			for oid := range s.collectReachable([]string{cur.oid}, 0) {
				extra[oid] = true
			}
			continue
		}
		included[cur.oid] = cur.gen
		if cur.gen >= maxGen {
			continue
		}
		for _, p := range parseCommit(obj.data).Parents {
			queue = append(queue, qitem{p, cur.gen + 1})
		}
	}
	boundary := make(map[string]bool)
	for oid, gen := range included {
		if gen == maxGen {
			boundary[oid] = true
		}
	}
	for _, oid := range plan.shallow {
		if gen, ok := included[oid]; ok && !boundary[oid] {
			_ = gen
			unshallow = append(unshallow, oid)
		}
	}
	objectsSet := make(map[string]bool)
	for oid := range extra {
		objectsSet[oid] = true
	}
	for oid := range included {
		if held[oid] {
			continue
		}
		objectsSet[oid] = true
		commit := s.getObject(oid)
		if commit == nil || commit.typ != objCommit {
			continue
		}
		tree := parseCommit(commit.data).Tree
		for entryOID := range s.collectReachable([]string{tree}, 0) {
			if !held[entryOID] {
				objectsSet[entryOID] = true
			}
		}
	}
	for oid := range extra {
		if held[oid] {
			delete(objectsSet, oid)
		}
	}
	for oid := range boundary {
		shallow = append(shallow, oid)
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return keysOf(objectsSet), shallow, unshallow
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func parseFetchArgs(lines []string) *fetchPlan {
	plan := &fetchPlan{}
	for _, line := range lines {
		t := strings.TrimSuffix(line, "\n")
		switch {
		case t == "" || t == "done" || t == "command=fetch" || t == "command=ls-refs":
		case strings.HasPrefix(t, "want "):
			plan.wants = append(plan.wants, strings.SplitN(t, " ", 2)[1])
		case strings.HasPrefix(t, "have "):
			plan.haves = append(plan.haves, strings.SplitN(t, " ", 2)[1])
		case strings.HasPrefix(t, "shallow "):
			plan.shallow = append(plan.shallow, t[8:])
		case strings.HasPrefix(t, "deepen "):
			if n, ok := atoiStrict(strings.TrimSpace(t[7:])); ok {
				plan.depth, plan.hasDepth = n, true
			}
		case strings.HasPrefix(t, "deepen-since "):
			if n, ok := atoiStrict(strings.TrimSpace(t[13:])); ok {
				plan.deepenSince = int64(n)
			}
		case strings.HasPrefix(t, "deepen-not "):
			plan.deepenNot = append(plan.deepenNot, t[11:])
		}
	}
	return plan
}

func v2FetchResponse(s *Store, repo *Repo, plan *fetchPlan) []byte {
	objects, shallow, unshallow := planFetch(s, plan)
	var pack []byte
	if len(objects) == 0 {
		pack = s.buildEmptyPack()
	} else {
		pack = s.buildPack(objects)
	}
	var out bytes.Buffer
	out.Write(pktLineString("acknowledgments\n"))
	heldHaves := []string{}
	for _, oid := range plan.haves {
		if s.getObject(oid) != nil {
			heldHaves = append(heldHaves, oid)
		}
	}
	if len(plan.haves) == 0 {
		out.Write(pktLineString("NAK\n"))
	} else {
		for _, oid := range heldHaves {
			out.Write(pktLineString("ACK " + oid + "\n"))
		}
		if len(heldHaves) == len(plan.haves) {
			out.Write(pktLineString("ready\n"))
		} else {
			out.Write(pktLineString("NAK\n"))
		}
	}
	out.Write(pktDelim)
	if len(shallow) > 0 || len(unshallow) > 0 {
		out.Write(pktLineString("shallow-info\n"))
		for _, oid := range shallow {
			out.Write(pktLineString("shallow " + oid + "\n"))
		}
		for _, oid := range unshallow {
			out.Write(pktLineString("unshallow " + oid + "\n"))
		}
		out.Write(pktDelim)
	}
	wanted := []refEntry{}
	for _, ref := range sortedRefs(repo) {
		for _, w := range plan.wants {
			if w == ref.oid {
				wanted = append(wanted, ref)
				break
			}
		}
	}
	if len(wanted) > 0 {
		out.Write(pktLineString("wanted-refs\n"))
		for _, ref := range wanted {
			out.Write(pktLineString(ref.oid + " " + ref.name + "\n"))
		}
		out.Write(pktDelim)
	}
	out.Write(pktLineString("packfile\n"))
	out.Write(sidebandPack(pack))
	return out.Bytes()
}

func v0FetchResponse(s *Store, repo *Repo, plan *fetchPlan) []byte {
	objects, shallow, unshallow := planFetch(s, plan)
	var pack []byte
	if len(objects) == 0 {
		pack = s.buildEmptyPack()
	} else {
		pack = s.buildPack(objects)
	}
	var out bytes.Buffer
	heldHaves := []string{}
	for _, oid := range plan.haves {
		if s.getObject(oid) != nil {
			heldHaves = append(heldHaves, oid)
		}
	}
	if len(plan.haves) == 0 {
		out.Write(pktLineString("NAK\n"))
	} else {
		for _, oid := range heldHaves {
			out.Write(pktLineString("ACK " + oid + "\n"))
		}
		if len(heldHaves) != len(plan.haves) {
			out.Write(pktLineString("NAK\n"))
		}
	}
	for _, oid := range shallow {
		out.Write(pktLineString("shallow " + oid + "\n"))
	}
	for _, oid := range unshallow {
		out.Write(pktLineString("unshallow " + oid + "\n"))
	}
	out.Write(sidebandPack(pack))
	return out.Bytes()
}

func handleInfoRefs(s *Store, owner, repoName string, req *Request, res *Response, service string) {
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		errReply(res, 404, "Not Found")
		return
	}
	wantsV2 := service == "git-upload-pack" && req.Headers["git-protocol"] == "version=2"
	switch service {
	case "git-upload-pack":
		if wantsV2 {
			sendBuffer(res, 200, v2Advertisement(), uploadAdvertise)
		} else {
			sendBuffer(res, 200, v0Advertisement(s, repo, "git-upload-pack"), uploadAdvertise)
		}
	case "git-receive-pack":
		sendBuffer(res, 200, v0Advertisement(s, repo, "git-receive-pack"), receiveAdvertise)
	default:
		errReply(res, 400, "unknown service")
	}
}

func handleUploadPack(s *Store, owner, repoName string, req *Request, res *Response, body []byte) {
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		errReply(res, 404, "Not Found")
		return
	}
	isV2 := req.Headers["git-protocol"] == "version=2"
	lines := pktTextLines(body)
	if isV2 {
		command := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "command=") {
				command = strings.TrimSpace(line[8:])
				break
			}
		}
		switch command {
		case "ls-refs":
			var prefixes []string
			peel, symrefs, unborn := false, false, false
			for _, line := range lines {
				t := strings.TrimSuffix(line, "\n")
				switch {
				case strings.HasPrefix(t, "ref-prefix "):
					prefixes = append(prefixes, t[11:])
				case t == "peel":
					peel = true
				case t == "symrefs":
					symrefs = true
				case t == "unborn":
					unborn = true
				}
			}
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
			var out bytes.Buffer
			refs := sortedRefs(repo)
			mainTip, hasMain := repo.Refs["refs/heads/main"]
			if len(refs) == 0 {
				if unborn && keep("HEAD") {
					out.Write(pktLineString("unborn HEAD symref-target:refs/heads/main\n"))
				}
			} else {
				if hasMain && keep("HEAD") {
					if symrefs {
						out.Write(pktLineString(mainTip + " HEAD symref-target:refs/heads/main\n"))
					} else {
						out.Write(pktLineString(mainTip + " HEAD\n"))
					}
				}
				for _, ref := range refs {
					if !keep(ref.name) {
						continue
					}
					line := ref.oid + " " + ref.name
					if peel {
						if peeled, ok := peelOf(s, ref.oid); ok {
							line += " peeled:" + peeled
						}
					}
					out.Write(pktLineString(line + "\n"))
				}
			}
			out.Write(pktFlush)
			sendBuffer(res, 200, out.Bytes(), uploadResult)
			return
		case "fetch":
			plan := parseFetchArgs(lines)
			sendBuffer(res, 200, v2FetchResponse(s, repo, plan), uploadResult)
			return
		default:
			errReply(res, 400, "unknown command")
			return
		}
	}
	plan := parseFetchArgs(lines)
	sendBuffer(res, 200, v0FetchResponse(s, repo, plan), uploadResult)
}

type receiveCmd struct {
	oldOID, newOID, ref string
	caps                []string
}

func handleReceivePack(s *Store, owner, repoName string, req *Request, res *Response, body []byte) {
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		errReply(res, 404, "Not Found")
		return
	}
	packets := readPktLines(body)
	var commands []receiveCmd
	packStart := len(body)
	pos := 0
	for _, p := range packets {
		if p.flush {
			packStart = pos + 4
			break
		}
		if p.delim {
			pos += 4
			continue
		}
		text := string(p.data)
		nul := strings.IndexByte(text, 0)
		head := text
		var caps []string
		if nul >= 0 {
			head = text[:nul]
			caps = strings.Fields(strings.TrimSpace(text[nul+1:]))
		}
		parts := strings.Fields(strings.TrimSpace(head))
		if len(parts) >= 3 {
			commands = append(commands, receiveCmd{parts[0], parts[1], parts[2], caps})
		}
		pos += 4 + len(p.data)
	}
	useSideband := len(commands) > 0 && containsStr(commands[0].caps, "side-band-64k")
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []result
	unpackOK := true
	unpackDetail := "ok"
	packBytes := body[packStart:]
	if len(packBytes) > 0 {
		entries, err := s.readPack(packBytes)
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for _, e := range entries {
				s.putObject(e.typ, e.data, e.oid)
			}
			s.depth1PackCache = make(map[string][]byte)
			s.packCache = make(map[string][]byte)
		}
	}
	if unpackOK {
		for _, cmd := range commands {
			current, exists := repo.Refs[cmd.ref]
			switch {
			case cmd.newOID == zeroOID:
				if !exists {
					results = append(results, result{cmd.ref, false, "remote ref does not exist"})
				} else {
					delete(repo.Refs, cmd.ref)
					results = append(results, result{cmd.ref, true, ""})
				}
			case cmd.oldOID == zeroOID:
				if exists {
					results = append(results, result{cmd.ref, false, "already exists"})
				} else if s.getObject(cmd.newOID) == nil {
					results = append(results, result{cmd.ref, false, "object not found"})
				} else {
					repo.Refs[cmd.ref] = cmd.newOID
					results = append(results, result{cmd.ref, true, ""})
				}
			case current != cmd.oldOID:
				results = append(results, result{cmd.ref, false, "non-fast-forward"})
			case cmd.newOID == cmd.oldOID:
				results = append(results, result{cmd.ref, true, ""})
			case !isAncestor(s, cmd.oldOID, cmd.newOID):
				results = append(results, result{cmd.ref, false, "non-fast-forward"})
			case s.getObject(cmd.newOID) == nil:
				results = append(results, result{cmd.ref, false, "object not found"})
			default:
				repo.Refs[cmd.ref] = cmd.newOID
				results = append(results, result{cmd.ref, true, ""})
			}
		}
	} else {
		for _, cmd := range commands {
			results = append(results, result{cmd.ref, false, "unpack failed"})
		}
	}
	var inner bytes.Buffer
	inner.Write(pktLineString("unpack " + unpackDetail + "\n"))
	for _, r := range results {
		if r.ok {
			inner.Write(pktLineString("ok " + r.ref + "\n"))
		} else {
			inner.Write(pktLineString("ng " + r.ref + " " + r.reason + "\n"))
		}
	}
	inner.Write(pktFlush)
	var out []byte
	if useSideband {
		out = sidebandPack(inner.Bytes())
	} else {
		out = inner.Bytes()
	}
	sendBuffer(res, 200, out, receiveResult)
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
