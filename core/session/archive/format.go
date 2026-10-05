package archive

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// The two record kinds a segment line can carry. A reader skips any other kind, so a later
// version can add one without breaking an older reader.
const (
	kindString = "s"
	kindEvent  = "e"
)

// record is one line of a segment.
//
// A string record carries H and V: a string the segment defines once, under its hash. An event
// record carries E, the event with its referenced fields blanked, and R, which of them were
// referenced. One struct for both kinds so a reader decodes a line without knowing its kind
// first.
type record struct {
	K string                 `json:"k"`
	H string                 `json:"h,omitempty"`
	V string                 `json:"v,omitempty"`
	E *pipeline.SessionEvent `json:"e,omitempty"`
	R *refs                  `json:"r,omitempty"`
}

// refs names, BY POSITION, which of an event's deduplicated strings were written as
// references. A nil entry means the value is inline in the event; an all-nil array is
// omitted.
//
// POSITIONAL RATHER THAN A SENTINEL STRING in the blanked field, because any sentinel is a
// string some prompt can contain. A position cannot collide with content.
//
// The fields are exactly the five core/session.Interner shares in memory — message content,
// the completion, tool descriptions, tool schemas, A2A part content — and nothing else, so the
// two dedups are of the same thing. MCP params and results stay inline for the interner's
// reason: they are maps, unmeasured on this workload.
type refs struct {
	M  []*string `json:"m,omitempty"`  // Inference.Messages[i].Content
	C  *string   `json:"c,omitempty"`  // Inference.Completion
	TD []*string `json:"td,omitempty"` // Inference.Tools[i].Description
	TP []*string `json:"tp,omitempty"` // Inference.Tools[i].Parameters
	AP []*string `json:"ap,omitempty"` // A2A.Parts[i].Content
}

// stringRec is a string a segment has not defined yet, keyed by its hash.
type stringRec struct{ h, v string }

// hashOf is a string's key in a segment: the first 16 bytes of its SHA-256, base64url without
// padding — 22 characters. 128 bits make an accidental collision across a session's strings
// negligible, and the strings are the user's own traffic, so there is no adversary to defend
// a shorter key against.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// dirName is the directory a session's segments live in: the hex of the first 16 bytes of the
// SHA-256 of its id. A session id is up to session.MaxSessionIDLen bytes of anything a client
// sent, so it cannot be a path element itself; its hash always can, at a fixed 32 characters.
func dirName(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:16])
}

// split returns e with every deduplicated field of at least session.InternMinLen bytes blanked,
// the references to them, and the strings among them not yet in seen, which it adds to seen.
//
// IT NEVER MUTATES e. The event is the store's — it is held there and served from there — and
// shares its slices with the store's copy, so split clones each extension and slice before
// blanking anything, as core/session.Interner.InternEvent does for the same reason.
//
// refs is nil when nothing was referenced, so a contentless event (a tunnel-open, a denial)
// costs no "r" key at all.
func split(e pipeline.SessionEvent, seen map[string]struct{}) (pipeline.SessionEvent, *refs, []stringRec) {
	var r refs
	var fresh []stringRec
	hit := false
	ref := func(s *string) *string {
		if len(*s) < session.InternMinLen {
			return nil
		}
		h := hashOf(*s)
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			fresh = append(fresh, stringRec{h: h, v: *s})
		}
		*s = ""
		hit = true
		return &h
	}

	if e.Inference != nil {
		cp := *e.Inference
		cp.Messages = slices.Clone(cp.Messages)
		r.M = make([]*string, len(cp.Messages))
		for i := range cp.Messages {
			r.M[i] = ref(&cp.Messages[i].Content)
		}
		r.C = ref(&cp.Completion)
		cp.Tools = slices.Clone(cp.Tools)
		r.TD = make([]*string, len(cp.Tools))
		r.TP = make([]*string, len(cp.Tools))
		for i := range cp.Tools {
			r.TD[i] = ref(&cp.Tools[i].Description)
			p := string(cp.Tools[i].Parameters)
			r.TP[i] = ref(&p)
			cp.Tools[i].Parameters = pipeline.RawJSON(p)
		}
		e.Inference = &cp
	}
	if e.A2A != nil {
		cp := *e.A2A
		cp.Parts = slices.Clone(cp.Parts)
		r.AP = make([]*string, len(cp.Parts))
		for i := range cp.Parts {
			r.AP[i] = ref(&cp.Parts[i].Content)
		}
		e.A2A = &cp
	}
	if !hit {
		return e, nil, fresh
	}
	r.M, r.TD, r.TP, r.AP = nilIfAllNil(r.M), nilIfAllNil(r.TD), nilIfAllNil(r.TP), nilIfAllNil(r.AP)
	return e, &r, fresh
}

func nilIfAllNil(xs []*string) []*string {
	for _, x := range xs {
		if x != nil {
			return xs
		}
	}
	return nil
}

// join restores the fields r references from table, in place. It reports false when a
// reference names a string the table does not hold, or a position the event does not have —
// both corruption, which a reader treats like a torn tail rather than serving a blanked field
// as if it were the original.
func join(e *pipeline.SessionEvent, r *refs, table map[string]string) bool {
	if r == nil {
		return true
	}
	set := func(dst *string, h *string) bool {
		if h == nil {
			return true
		}
		v, ok := table[*h]
		if ok {
			*dst = v
		}
		return ok
	}
	if e.Inference == nil {
		if r.M != nil || r.C != nil || r.TD != nil || r.TP != nil {
			return false
		}
	} else {
		inf := e.Inference
		if len(r.M) > len(inf.Messages) || len(r.TD) > len(inf.Tools) || len(r.TP) > len(inf.Tools) {
			return false
		}
		for i, h := range r.M {
			if !set(&inf.Messages[i].Content, h) {
				return false
			}
		}
		if !set(&inf.Completion, r.C) {
			return false
		}
		for i, h := range r.TD {
			if !set(&inf.Tools[i].Description, h) {
				return false
			}
		}
		for i, h := range r.TP {
			p := string(inf.Tools[i].Parameters)
			if !set(&p, h) {
				return false
			}
			inf.Tools[i].Parameters = pipeline.RawJSON(p)
		}
	}
	if e.A2A == nil {
		return r.AP == nil
	}
	if len(r.AP) > len(e.A2A.Parts) {
		return false
	}
	for i, h := range r.AP {
		if !set(&e.A2A.Parts[i].Content, h) {
			return false
		}
	}
	return true
}

// stringLine and eventLine are the two lines a segment writes, each with its trailing newline.
func stringLine(s stringRec) ([]byte, error) {
	return appendLine(record{K: kindString, H: s.h, V: s.v})
}

func eventLine(e pipeline.SessionEvent, r *refs) ([]byte, error) {
	return appendLine(record{K: kindEvent, E: &e, R: r})
}

func appendLine(rec record) ([]byte, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
