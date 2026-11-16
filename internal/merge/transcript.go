package merge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Line is one parsed transcript line.
type Line struct {
	Raw      []byte // without the trailing newline
	Key      string
	UUID     string
	Parents  []string // parentUuid, logicalParentUuid
	SortTime time.Time
	Valid    bool // complete JSON object
}

type lineFields struct {
	Type          string `json:"type"`
	UUID          string `json:"uuid"`
	ParentUUID    string `json:"parentUuid"`
	LogicalParent string `json:"logicalParentUuid"`
	Timestamp     string `json:"timestamp"`
}

// ParseTranscript splits a normalized transcript (complete lines only) and
// gives every line its merge key (docs/sync-engine.md, 8-1):
//   - lines with a uuid: "u:<uuid>";
//   - others: "h:<anchor>:<sha256>:<n>", where anchor is the uuid of the
//     nearest earlier line that has one and n counts repeats of the same
//     content after that anchor.
func ParseTranscript(content []byte) []Line {
	var out []Line
	anchor := "^"
	repeats := map[string]int{}
	var lastTime time.Time
	for _, raw := range bytes.Split(content, []byte{'\n'}) {
		if len(raw) == 0 {
			continue
		}
		l := Line{Raw: raw}
		var f lineFields
		if json.Unmarshal(raw, &f) == nil {
			l.Valid = true
			l.UUID = f.UUID
			for _, p := range []string{f.ParentUUID, f.LogicalParent} {
				if p != "" {
					l.Parents = append(l.Parents, p)
				}
			}
			if ts, err := time.Parse(time.RFC3339Nano, f.Timestamp); err == nil {
				lastTime = ts
			}
		}
		l.SortTime = lastTime
		if l.UUID != "" {
			l.Key = "u:" + l.UUID
			anchor = l.UUID
			repeats = map[string]int{}
		} else {
			s := sha256.Sum256(raw)
			h := hex.EncodeToString(s[:12])
			n := repeats[h]
			repeats[h] = n + 1
			l.Key = "h:" + anchor + ":" + h + ":" + strconv.Itoa(n)
		}
		out = append(out, l)
	}
	return out
}

// TranscriptStats describes a transcript for the manifest and format checks.
type TranscriptStats struct {
	Lines        int
	Invalid      int // lines that are not JSON objects
	MissingUUIDs int // user/assistant lines without uuid
}

// Stats inspects a transcript.
func Stats(content []byte) TranscriptStats {
	var s TranscriptStats
	for _, raw := range bytes.Split(content, []byte{'\n'}) {
		if len(raw) == 0 {
			continue
		}
		s.Lines++
		var f lineFields
		if json.Unmarshal(raw, &f) != nil {
			s.Invalid++
			continue
		}
		if (f.Type == "user" || f.Type == "assistant") && f.UUID == "" {
			s.MissingUUIDs++
		}
	}
	return s
}

// Transcripts merges versions of one transcript: the union of their lines,
// no duplicates, every version's order kept, deterministic (the same output
// whatever the order of the inputs).
func Transcripts(versions ...[]byte) []byte {
	parsed := make([][]Line, 0, len(versions))
	for _, v := range versions {
		parsed = append(parsed, ParseTranscript(v))
	}
	// Fast path: one version's keys are a prefix of another's.
	if longest, ok := prefixChain(parsed); ok {
		return render(parsed[longest])
	}

	type node struct {
		line  Line
		succ  map[string]bool
		indeg int
	}
	nodes := map[string]*node{}
	addEdge := func(a, b string) {
		if a == b {
			return
		}
		na := nodes[a]
		if !na.succ[b] {
			na.succ[b] = true
			nodes[b].indeg++
		}
	}
	for _, ls := range parsed {
		for _, l := range ls {
			n, ok := nodes[l.Key]
			if !ok {
				nodes[l.Key] = &node{line: l, succ: map[string]bool{}}
				continue
			}
			// Same key, different bytes: keep the longer, then the larger.
			if c := bytes.Compare(l.Raw, n.line.Raw); c != 0 &&
				(len(l.Raw) > len(n.line.Raw) || len(l.Raw) == len(n.line.Raw) && c > 0) {
				n.line.Raw = l.Raw
			}
			if l.SortTime.Before(n.line.SortTime) || n.line.SortTime.IsZero() {
				n.line.SortTime = l.SortTime
			}
		}
	}
	for _, ls := range parsed {
		for i := 1; i < len(ls); i++ {
			addEdge(ls[i-1].Key, ls[i].Key)
		}
		for _, l := range ls {
			for _, p := range l.Parents {
				if _, ok := nodes["u:"+p]; ok {
					addEdge("u:"+p, l.Key)
				}
			}
		}
	}
	// Edges may form cycles only if inputs disagree on order; the loop below
	// still emits every node exactly once.
	less := func(a, b *node) bool {
		if !a.line.SortTime.Equal(b.line.SortTime) {
			return a.line.SortTime.Before(b.line.SortTime)
		}
		return a.line.Key < b.line.Key
	}
	emitted := map[string]bool{}
	ready := map[string]*node{}
	for k, n := range nodes {
		if n.indeg == 0 {
			ready[k] = n
		}
	}
	var out [][]byte
	var last *node
	for len(emitted) < len(nodes) {
		var best *node
		bestFollows := false
		for k, n := range ready {
			follows := last != nil && last.succ[k]
			if best == nil || follows && !bestFollows || follows == bestFollows && less(n, best) {
				best, bestFollows = n, follows
			}
		}
		if best == nil { // cycle: take the smallest remaining node
			for k, n := range nodes {
				if !emitted[k] && (best == nil || less(n, best)) {
					best = n
				}
			}
		}
		k := best.line.Key
		emitted[k] = true
		delete(ready, k)
		for s := range best.succ {
			if emitted[s] {
				continue
			}
			nodes[s].indeg--
			if nodes[s].indeg == 0 {
				ready[s] = nodes[s]
			}
		}
		out = append(out, best.line.Raw)
		last = best
	}
	return render2(out)
}

func prefixChain(parsed [][]Line) (int, bool) {
	longest := 0
	for i := range parsed {
		if len(parsed[i]) > len(parsed[longest]) {
			longest = i
		}
	}
	for i := range parsed {
		if i == longest {
			continue
		}
		for j, l := range parsed[i] {
			if parsed[longest][j].Key != l.Key {
				return 0, false
			}
		}
	}
	return longest, true
}

func render(ls []Line) []byte {
	raws := make([][]byte, len(ls))
	for i, l := range ls {
		raws[i] = l.Raw
	}
	return render2(raws)
}

func render2(raws [][]byte) []byte {
	var b bytes.Buffer
	for _, r := range raws {
		b.Write(r)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// DesktopActivity reads lastActivityAt from a desktop session entry.
func DesktopActivity(content []byte) (int64, error) {
	var v struct {
		LastActivityAt json.Number `json:"lastActivityAt"`
	}
	if err := json.Unmarshal(content, &v); err != nil {
		return 0, err
	}
	if v.LastActivityAt == "" {
		return 0, fmt.Errorf("no lastActivityAt")
	}
	if n, err := v.LastActivityAt.Int64(); err == nil {
		return n, nil
	}
	f, err := v.LastActivityAt.Float64()
	return int64(f), err
}
