package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/notnil/chess"

	"chess-demo/internal/prox"
)

// app holds the game state. All mutation happens on the main goroutine;
// the poll goroutine and stdin reader only deliver messages.
type app struct {
	client *prox.Client
	game   *chess.Game
	sans   []string        // SAN of every applied move, for the status line
	status string          // last status line (hints, errors, rejections)
	pend   []string        // human moves typed before their turn
	seen   map[string]bool // dedupe on event id
	over   bool

	// hook, when set (tests), captures emitted events instead of sending
	// them through the proxenos CLI.
	hook func(typ string, body any)
}

func newApp(c *prox.Client) *app {
	return &app{
		client: c,
		game:   chess.NewGame(),
		seen:   map[string]bool{},
	}
}

// ── outbound events ──────────────────────────────────────────────────────

func (a *app) emitStarted() {
	a.send("game.started", map[string]any{
		"fen":        a.game.FEN(),
		"humanColor": "white",
	})
}

// emitPosition announces the board after every applied move (both sides).
func (a *app) emitPosition(lastSan string) {
	a.send("game.position", map[string]any{
		"fen":      a.game.FEN(),
		"lastMove": lastSan,
		"turn":     colorName(a.game.Position().Turn()),
		"check":    a.checkNow(),
		"ascii":    renderBoard(a.game),
	})
}

func (a *app) emitOver() {
	out := a.game.Outcome()
	a.send("game.over", map[string]any{
		"result": out.String(),
		"reason": strings.ToLower(a.game.Method().String()),
	})
}

// checkNow: the library tags a move with Check when it puts the opponent
// in check (checkmate implies check), so the last move's tag is exactly
// the current position's check status.
func (a *app) checkNow() bool {
	hist := a.game.MoveHistory()
	if len(hist) == 0 {
		return false
	}
	return hist[len(hist)-1].Move.HasTag(chess.Check)
}

// ── move application (shared by human input and agent events) ────────────

// applyMove plays a validated-legal move and emits the follow-up events.
// Returns false if the game ended (the caller then exits).
func (a *app) applyMove(m *chess.Move, by string) {
	san := algebraic.Encode(a.game.Position(), m)
	from, to := m.S1().String(), m.S2().String()
	if err := a.game.Move(m); err != nil {
		a.status = fmt.Sprintf("move rejected: %v", err)
		return
	}
	a.sans = append(a.sans, san)
	a.send("game.move", map[string]any{
		"from": from,
		"to":   to,
		"san":  san,
		"by":   by,
	})
	a.emitPosition(san)
}

// settle is called after every state change: finishes the game if over,
// otherwise drains human moves queued while it was Black's turn.
// Returns false when the app should exit.
func (a *app) settle() bool {
	if a.game.Outcome() != chess.NoOutcome {
		a.finishGame()
		return false
	}
	for a.game.Position().Turn() == chess.White && len(a.pend) > 0 {
		line := a.pend[0]
		a.pend = a.pend[1:]
		if !a.humanMove(line) {
			return false
		}
	}
	return true
}

func (a *app) finishGame() {
	a.over = true
	a.emitOver()
}

// ── human input ──────────────────────────────────────────────────────────

// humanMove parses and plays one line of human input. Returns false when
// the game ended.
func (a *app) humanMove(line string) bool {
	if a.game.Outcome() != chess.NoOutcome {
		a.status = "game is over"
		return true
	}
	if a.game.Position().Turn() != chess.White {
		a.pend = append(a.pend, line)
		a.status = "not your turn — Black is thinking"
		return true
	}
	if strings.EqualFold(strings.TrimSpace(line), "resign") {
		a.game.Resign(chess.White)
		a.status = "you resigned"
		a.finishGame()
		return false
	}
	m, err := parseMove(a.game, line)
	if err != nil {
		a.status = err.Error()
		return true
	}
	a.applyMove(m, "white")
	return a.settle()
}

// parseMove accepts SAN ("Nf3", "e4", "e8=Q") or from/to ("e2e4", "e7e8q").
func parseMove(g *chess.Game, s string) (*chess.Move, error) {
	s = strings.TrimSpace(s)
	pos := g.Position()
	if m, err := algebraic.Decode(pos, s); err == nil {
		return m, nil
	}
	if m, err := uci.Decode(pos, s); err == nil {
		return m, nil
	}
	return nil, fmt.Errorf("illegal or unparseable move %q", s)
}

// ── inbound: the accepts half of the contract ────────────────────────────

// applyInbound applies one event from the stream. Structural validation
// happens here; chess rules are checked against the live game. Every
// can't-apply accepted verb gets a loud action.rejected.
func (a *app) applyInbound(ev *prox.Event) {
	if a.seen[ev.ID] || ev.Src == a.client.Src {
		return // dedupe, and never re-apply our own emissions
	}
	a.seen[ev.ID] = true
	switch ev.Type {
	case "chess.move":
		a.applyAgentMove(ev)
	case "chess.resign":
		a.applyAgentResign(ev)
	case "chess.hint":
		if text := hintText(ev.Body); text != "" {
			a.status = "hint: " + text
		}
	default:
		// Unknown types are allowed on a shared stream; only our accepts
		// vocabulary is actionable.
	}
}

func (a *app) applyAgentMove(ev *proxEvent) {
	if a.game.Outcome() != chess.NoOutcome {
		a.reject("chess.move", "game is over")
		return
	}
	if a.game.Position().Turn() != chess.Black {
		a.reject("chess.move", "not Black's turn — White to move")
		return
	}
	var body struct {
		From, To, San string
	}
	if err := json.Unmarshal(ev.Body, &body); err != nil {
		a.reject("chess.move", "malformed body: "+err.Error())
		return
	}
	var m *chess.Move
	switch {
	case body.San != "":
		if mv, err := algebraic.Decode(a.game.Position(), body.San); err == nil {
			m = mv
		} else {
			a.reject("chess.move", "illegal SAN move "+body.San+": "+err.Error())
			return
		}
	case body.From != "" && body.To != "":
		if mv, err := uci.Decode(a.game.Position(), body.From+body.To); err == nil {
			m = mv
		} else {
			a.reject("chess.move", fmt.Sprintf("illegal move %s%s: %v", body.From, body.To, err))
			return
		}
	default:
		a.reject("chess.move", "body needs from/to squares or san")
		return
	}
	a.applyMove(m, "black")
	a.settle()
}

func (a *app) applyAgentResign(ev *proxEvent) {
	if a.game.Outcome() != chess.NoOutcome {
		a.reject("chess.resign", "game is over")
		return
	}
	a.game.Resign(chess.Black)
	a.status = "Black resigned"
	a.finishGame()
}

// hintText extracts the hint text: {"text": …}, {"hint": …}, or a bare
// JSON string body.
func hintText(body json.RawMessage) string {
	var obj struct{ Text, Hint string }
	if err := json.Unmarshal(body, &obj); err == nil {
		if obj.Text != "" {
			return obj.Text
		}
		return obj.Hint
	}
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		return s
	}
	return ""
}

var (
	algebraic = chess.AlgebraicNotation{}
	uci       = chess.UCINotation{}
)

func colorName(c chess.Color) string {
	switch c {
	case chess.White:
		return "white"
	case chess.Black:
		return "black"
	default:
		return "none"
	}
}
