package main

import (
	"encoding/json"
	"fmt"

	"chess-demo/internal/prox"
)

// proxEvent aliases the wire envelope type under the name app code uses.
type proxEvent = prox.Event

// proxLog is the integration error sink, set by run() to a log file — the
// app owns the terminal, so send failures never go to stderr.
var proxLog = func(string, ...any) {}

// send emits one event, best-effort: a proxenos failure never crashes the
// game; it lands in the log file.
func (a *app) send(typ string, body any) {
	if a.hook != nil {
		a.hook(typ, body)
		return
	}
	if _, err := a.client.Send(typ, body); err != nil {
		proxLog("send %s failed: %v", typ, err)
	}
}

// reject emits action.rejected — a loud failure beats a silent one.
func (a *app) reject(action, reason string) {
	a.send("action.rejected", map[string]any{"action": action, "reason": reason})
}

// decodeRecord parses one raw NDJSON record into an event envelope.
func decodeRecord(rec []byte) (*proxEvent, error) {
	var ev proxEvent
	if err := json.Unmarshal(rec, &ev); err != nil {
		return nil, err
	}
	if ev.ID == "" || ev.Type == "" {
		return nil, fmt.Errorf("record missing id or type")
	}
	return &ev, nil
}
