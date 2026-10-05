package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ClearResult is DELETE /v1/sessions' answer: what went from memory, and what went from disk.
type ClearResult struct {
	Sessions         int   `json:"sessions"`
	ArchivedSessions int   `json:"archivedSessions"`
	Bytes            int64 `json:"bytes"`
	// ArchiveError is why the disk half failed. Memory is cleared even then.
	ArchiveError string `json:"archiveError,omitempty"`
}

// ErrClearUnsupported is a proxy that predates DELETE /v1/sessions.
var ErrClearUnsupported = errors.New("this Cortex predates clearing sessions; upgrade it")

// ErrClearRefused is a 403: the proxy does not accept a clear from here. The error carries its
// reason.
var ErrClearRefused = errors.New("clear refused")

// clearTimeout bounds ClearSessions when the caller set no deadline. Longer than
// restDefaultTimeout because the server waits up to 10s for its archive before answering; a
// client giving up at the same 10s would report a clear that happened as one that failed.
var clearTimeout = 30 * time.Second

// ClearSessions asks the proxy to clear every session, in memory and on disk. A 500 still
// returns the counts — memory was cleared — with ArchiveError saying why the disk half was not.
//
// ITS OWN REQUEST, not getBody's: that one is GET-only, and it keeps the body of a 400 alone,
// where this has to read the body of a 403 and of a 500 too.
func (c *Client) ClearSessions(ctx context.Context) (ClearResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, clearTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint+"/v1/sessions", nil)
	if err != nil {
		return ClearResult{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ClearResult{}, err
	}
	defer resp.Body.Close()
	// Bounded like every other body this client reads from a peer it cannot verify. 4KB rather
	// than badRequestDetail's 512, because archiveError carries the archive's paths, and a body
	// cut short of its closing brace would lose the counts with it.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusInternalServerError:
		var res ClearResult
		if err := json.Unmarshal(body, &res); err != nil {
			return ClearResult{}, fmt.Errorf("/v1/sessions: unexpected status %d", resp.StatusCode)
		}
		res.ArchiveError = serverText(res.ArchiveError)
		return res, nil
	case http.StatusForbidden:
		if detail := badRequestDetail(body); detail != "" {
			return ClearResult{}, fmt.Errorf("%w: %s", ErrClearRefused, detail)
		}
		return ClearResult{}, ErrClearRefused
	case http.StatusMethodNotAllowed, http.StatusNotFound:
		return ClearResult{}, ErrClearUnsupported
	}
	return ClearResult{}, fmt.Errorf("/v1/sessions: unexpected status %d", resp.StatusCode)
}
