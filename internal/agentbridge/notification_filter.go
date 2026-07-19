package agentbridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sync/atomic"
)

// sessionLoadNotificationFilter removes replay-only session updates from the
// ACP byte stream before they enter the SDK's bounded notification queue.
// It is active only while session/load is in flight; all other JSON-RPC
// traffic, including responses and inbound requests, passes through unchanged.
type sessionLoadNotificationFilter struct {
	source      *bufio.Reader
	suppress    *atomic.Bool
	pending     []byte
	deferredErr error
	dropped     atomic.Uint64
}

func newSessionLoadNotificationFilter(source io.Reader, suppress *atomic.Bool) *sessionLoadNotificationFilter {
	return &sessionLoadNotificationFilter{
		source:   bufio.NewReader(source),
		suppress: suppress,
	}
}

func (r *sessionLoadNotificationFilter) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.deferredErr != nil {
			err := r.deferredErr
			r.deferredErr = nil
			return 0, err
		}
		line, err := r.source.ReadBytes('\n')
		if len(line) > 0 {
			if r.suppress.Load() && isSessionUpdateNotification(line) {
				r.dropped.Add(1)
			} else {
				r.pending = line
			}
		}
		if err != nil {
			if len(r.pending) == 0 {
				return 0, err
			}
			r.deferredErr = err
		}
	}

	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *sessionLoadNotificationFilter) Dropped() uint64 {
	return r.dropped.Load()
}

func isSessionUpdateNotification(line []byte) bool {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &envelope); err != nil || len(envelope.ID) != 0 {
		return false
	}
	return envelope.Method == "session/update" || envelope.Method == "_x.ai/session/update"
}
