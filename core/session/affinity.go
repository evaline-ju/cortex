package session

import (
	"log/slog"
	"strings"
	"time"
)

// Client affinity: filing a request that carries no session header under a session of
// the SAME coding agent, rather than under ActiveSession(), the single global "most
// recently updated" id. With two agents running, ActiveSession() files each one's
// header-less calls — Bob's startup probes and task classifier, Claude Code's WebFetch —
// into whichever session spoke last, which is the other agent's half the time.
//
// Everything here is inert until a listener calls Claim, which only one configured with
// session.client_affinity does. A store nothing claims from answers SessionForClient with
// "" for every unknown client and a pending id for every known one, and holds no state.

// PendingPrefix marks the bucket a known coding agent's header-less requests collect in
// before its first session header arrives. See Claim for how that bucket is adopted.
const PendingPrefix = "pending:"

// PendingSessionID is the pending bucket for client.
func PendingSessionID(client string) string { return PendingPrefix + client }

// Rekeyer is optionally implemented by a Recorder that keeps per-session state, so that
// state follows a rename. Called under the store's write lock, like Record, once for every
// rename the store makes — Adopt's pending bucket → session, and Rekey's A2A default →
// contextId merge — and never for a rename it refused.
type Rekeyer interface {
	Rekeyed(oldID, newID string)
}

// Claim records that client named sessionID through its own session header, which is what
// makes it that client's session for SessionForClient. First claim wins: a session is its
// creator's, and a second agent quoting the same id does not take it over.
//
// It also ADOPTS the client's pending bucket into sessionID when sessionID holds nothing
// yet, so the calls an agent makes before its first header — Bob's /admin/v1/profile,
// /model/info and task classifier — land in the session they belong to. Called at hydration,
// BEFORE the headered request is appended, which is what makes "holds nothing yet" the common
// case rather than a race. When sessionID already exists the pending bucket is left as its
// own row: merging two event histories is not something the store can do honestly.
func (s *Store) Claim(sessionID, client string) {
	if sessionID == "" || client == "" || strings.HasPrefix(sessionID, PendingPrefix) {
		return
	}
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owners == nil {
		s.owners = make(map[string]string)
	}
	owner, owned := s.owners[sessionID]
	if !owned {
		s.pruneOwnersLocked()
		s.owners[sessionID], owner = client, client
	}
	if owner == client {
		// Adoption into a session that already holds events cannot succeed, so past the
		// session's first claim it is not tried: every headered request would log it.
		if _, holds := s.sessions[sessionID]; !owned || !holds {
			s.adoptLocked(PendingSessionID(client), sessionID)
		}
	}
}

// Adopt renames pendingID to id the way Rekey does — every Rekeyer recorder is told, so the
// usage aggregator's per-session figures follow — and additionally records the adoption, so
// a response still in flight under pendingID lands in id (see followAdoptedLocked). Reports
// false, changing nothing, when pendingID does not exist or id already does.
func (s *Store) Adopt(pendingID, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptLocked(pendingID, id)
}

func (s *Store) adoptLocked(pendingID, id string) bool {
	if _, pending := s.sessions[pendingID]; !pending {
		return false
	}
	if !s.rekeyLocked(pendingID, id) {
		slog.Info("session: pending bucket not adopted, target already holds events",
			"pending", pendingID, "session", id)
		return false
	}
	// The request that was in flight when its client's first header arrived is still pinned
	// to the pending id — its response is recorded under pctx.OutboundSessionID. Without the
	// redirect that response would recreate the bucket just adopted, as a stray row of orphan
	// responses. See followAdoptedLocked.
	if s.adopted == nil {
		s.adopted = make(map[string]string)
	}
	s.adopted[pendingID] = id
	// rekeyLocked has told every Rekeyer.
	return true
}

// followAdoptedLocked maps a pending id that has been adopted to the session that adopted
// it, for as long as that session exists. Any other id comes back unchanged.
func (s *Store) followAdoptedLocked(id string) string {
	to, ok := s.adopted[id]
	if !ok {
		return id
	}
	if _, live := s.sessions[to]; live {
		return to
	}
	delete(s.adopted, id)
	return id
}

// ambiguityWindow is how recently a known agent must have sent traffic to count toward
// SessionForClient's ambiguous case. Expiry cannot serve: session.ttl defaults to never,
// so one Bob run would otherwise send every unknown client to default for the rest of the
// proxy's life.
const ambiguityWindow = 5 * time.Minute

// SessionForClient is where a request with no session header goes under client affinity,
// or "" to fall back to ActiveSession():
//
//  1. A known client (client != ""): its newest live session, else its pending bucket.
//  2. An unknown client while two or more known clients have each sent traffic within
//     ambiguityWindow: the default bucket, because the owner is ambiguous and guessing
//     files it into one of theirs.
//  3. Anything else: "", so ActiveSession() answers exactly as it does without affinity.
//     This is the in-cluster case — an A2A agent is no known coding agent, and
//     ActiveSession() is what ties its outbound calls to the inbound turn that caused them.
func (s *Store) SessionForClient(client string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.clock()
	if client != "" {
		var newest string
		var at time.Time
		for id, owner := range s.owners {
			sess, ok := s.sessions[id]
			if owner != client || !ok || s.isExpired(sess, now) {
				continue
			}
			if newest == "" || sess.UpdatedAt.After(at) {
				newest, at = id, sess.UpdatedAt
			}
		}
		if newest != "" {
			return newest
		}
		return PendingSessionID(client)
	}
	seen := make(map[string]struct{}, 2)
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) || now.Sub(sess.UpdatedAt) > ambiguityWindow {
			continue
		}
		owner := s.owners[id]
		if owner == "" {
			var ok bool
			if owner, ok = pendingOwner(id); !ok {
				continue
			}
		}
		seen[owner] = struct{}{}
		if len(seen) >= 2 {
			return DefaultSessionID
		}
	}
	return ""
}

// pruneOwnersLocked drops claims for sessions the store no longer holds. A claim is made at
// hydration, before anything is appended, so a request rejected there leaves one behind that
// neither cleanup nor eviction will ever see. Swept only past twice the session cap, so the
// common Claim pays nothing.
func (s *Store) pruneOwnersLocked() {
	limit := 2 * s.maxSessions
	if limit <= 0 { // an uncapped store: sweep at a fixed size instead
		limit = 256
	}
	if len(s.owners) < limit {
		return
	}
	for id := range s.owners {
		if _, ok := s.sessions[id]; !ok {
			delete(s.owners, id)
		}
	}
}
