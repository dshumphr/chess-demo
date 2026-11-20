// Package prox is the app-side client for the proxenos middleware.
//
// Outbound events go through the proxenos CLI (`send`), which stamps id/ts
// and validates the envelope. Inbound events are read by polling the stream
// file directly (see reader.go) — no long-lived child process.
//
// The stream is the API: one append-only NDJSON log. See
// proxenos/docs/app-integration.md and DESIGN.md (reader rules).
package prox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Client shells out to the proxenos binary for sends and stream setup.
type Client struct {
	// Bin is the resolved path of the proxenos binary.
	Bin string
	// Src is this app's identity, passed as --as on every send (e.g. "app:chess").
	Src string
	// Stream is the stream name.
	Stream string
}

// Event is the proxenos wire envelope. Unknown fields are ignored.
type Event struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	TS   string          `json:"ts"`
	Src  string          `json:"src"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// FindBin resolves the proxenos binary on PATH.
func FindBin() (string, error) {
	p, err := exec.LookPath("proxenos")
	if err != nil {
		return "", fmt.Errorf("proxenos binary not on PATH: %w", err)
	}
	return p, nil
}

// EnsureStream probes the stream and creates it (seeded with the contract)
// only if it is missing. Idempotent per the integration guide: probe with
// `tail --tail 1`, create only on failure; a create racing an existing
// stream is treated as success.
func (c *Client) EnsureStream(contractJSON []byte) error {
	probe := exec.Command(c.Bin, "tail", c.Stream, "--tail", "1")
	probe.Stdout = nil
	probe.Stderr = nil
	if probe.Run() == nil {
		return nil
	}
	tmp, err := os.CreateTemp("", "proxenos-contract-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(contractJSON); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	out, err := exec.Command(c.Bin, "new", c.Stream, "--contract", tmp.Name()).CombinedOutput()
	if err != nil && !bytes.Contains(out, []byte("already exists")) {
		return fmt.Errorf("creating stream %q: %v: %s", c.Stream, err, out)
	}
	return nil
}

// ResolveStreamPath asks proxenos where the stream file lives (`ls --json`)
// rather than recomputing the data dir by hand — proxenos config can move it.
func (c *Client) ResolveStreamPath() (string, error) {
	out, err := exec.Command(c.Bin, "ls", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("proxenos ls --json: %w", err)
	}
	var infos []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(out, &infos); err != nil {
		return "", fmt.Errorf("parsing proxenos ls --json: %w", err)
	}
	for _, in := range infos {
		if in.Name == c.Stream {
			return in.Path, nil
		}
	}
	return "", fmt.Errorf("stream %q not found in proxenos ls", c.Stream)
}

// Send appends one event. The CLI stamps id/ts; body must be valid JSON
// (one line, no embedded newlines — proxenos enforces one write(2) per
// record, and the CLI rejects bad bodies).
func (c *Client) Send(typ string, body any) (string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(c.Bin, "send", c.Stream, typ, string(raw), "--as", c.Src)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("proxenos send %s: %v: %s", typ, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.String(), nil
}

// Reject emits an action.rejected record. A loud failure beats a silent one.
func (c *Client) Reject(action, reason string) {
	_, _ = c.Send("action.rejected", map[string]any{
		"action": action,
		"reason": reason,
	})
}
