package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/notnil/chess"

	"chess-demo/internal/prox"
)

// newTestApp builds an app whose emissions are captured in a slice.
func newTestApp(t *testing.T) (*app, *[]emitted) {
	t.Helper()
	a := newApp(&prox.Client{Src: appSrc, Stream: "chess"})
	out := &[]emitted{}
	a.hook = func(typ string, body any) {
		*out = append(*out, emitted{typ, body})
	}
	return a, out
}

type emitted struct {
	typ  string
	body any
}

func evBody(t *testing.T, e emitted) map[string]any {
	t.Helper()
	raw, err := json.Marshal(e.body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func lastEmitted(out *[]emitted, typ string) (emitted, bool) {
	for i := len(*out) - 1; i >= 0; i-- {
		if (*out)[i].typ == typ {
			return (*out)[i], true
		}
	}
	return emitted{}, false
}

func TestAgentMoveFoolsMate(t *testing.T) {
	a, out := newTestApp(t)
	if !a.humanMove("f3") {
		t.Fatal("game ended too early")
	}
	if !a.humanMove("g4") {
		t.Fatal("game ended too early")
	}
	a.applyInbound(&proxEvent{ID: "a0", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"e5"}`)})
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"Qh4#"}`)})
	if a.game.Outcome() != chess.BlackWon {
		t.Fatalf("expected Black win, got %s", a.game.Outcome())
	}
	if !a.over {
		t.Fatal("over flag not set")
	}
	over, ok := lastEmitted(out, "game.over")
	if !ok {
		t.Fatal("game.over not emitted")
	}
	b := evBody(t, over)
	if b["result"] != "0-1" || b["reason"] != "checkmate" {
		t.Fatalf("bad game.over body: %v", b)
	}
	// A game.position must have been emitted after Black's move too.
	pos, ok := lastEmitted(out, "game.position")
	if !ok {
		t.Fatal("game.position not emitted after agent move")
	}
	if b := evBody(t, pos); b["lastMove"] != "Qh4#" {
		t.Fatalf("bad lastMove: %v", b)
	}
}

func TestAgentMoveFromTo(t *testing.T) {
	a, out := newTestApp(t)
	if !a.humanMove("e4") {
		t.Fatal("humanMove ended game")
	}
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"from":"e7","to":"e5"}`)})
	if got := a.sans; len(got) != 2 || got[1] != "e5" {
		t.Fatalf("moves: %v", got)
	}
	mv, ok := lastEmitted(out, "game.move")
	if !ok {
		t.Fatal("game.move not emitted")
	}
	b := evBody(t, mv)
	if b["from"] != "e7" || b["to"] != "e5" || b["san"] != "e5" || b["by"] != "black" {
		t.Fatalf("bad game.move body: %v", b)
	}
}

func TestAgentMoveIllegalRejected(t *testing.T) {
	a, out := newTestApp(t)
	if !a.humanMove("e4") {
		t.Fatal("humanMove ended game")
	}
	// Black to move: Qd8h4-style illegal move (queen blocked on d8... use
	// a clearly illegal SAN for Black: Ne8?? knight can't reach e8? — use
	// a plain impossible destination).
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"Qh4"}`)})
	if a.game.Position().Turn() != chess.Black {
		t.Fatal("illegal move was applied")
	}
	rej, ok := lastEmitted(out, "action.rejected")
	if !ok {
		t.Fatal("action.rejected not emitted")
	}
	b := evBody(t, rej)
	if b["action"] != "chess.move" || !strings.Contains(b["reason"].(string), "illegal") {
		t.Fatalf("bad rejection: %v", b)
	}
}

func TestAgentMoveOutOfTurnRejected(t *testing.T) {
	a, out := newTestApp(t)
	// White to move: a Black move attempt is out of turn.
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"e5"}`)})
	if _, ok := lastEmitted(out, "action.rejected"); !ok {
		t.Fatal("out-of-turn move not rejected")
	}
	if a.game.Position().Turn() != chess.White {
		t.Fatal("out-of-turn move was applied")
	}
}

func TestAgentMoveMalformedRejected(t *testing.T) {
	a, out := newTestApp(t)
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{}`)})
	if _, ok := lastEmitted(out, "action.rejected"); !ok {
		t.Fatal("malformed move not rejected")
	}
}

func TestDedupeOnEventID(t *testing.T) {
	a, _ := newTestApp(t)
	if !a.humanMove("e4") {
		t.Fatal("humanMove ended game")
	}
	ev := &proxEvent{ID: "same-id", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"e5"}`)}
	a.applyInbound(ev)
	a.applyInbound(ev) // retry/reconnect duplicate
	if got := a.sans; len(got) != 2 {
		t.Fatalf("duplicate applied: %v", got)
	}
}

func TestOwnEventsIgnored(t *testing.T) {
	a, _ := newTestApp(t)
	if !a.humanMove("e4") {
		t.Fatal("humanMove ended game")
	}
	a.applyInbound(&proxEvent{ID: "x", Src: appSrc, Type: "chess.move", Body: json.RawMessage(`{"san":"e5"}`)})
	if a.game.Position().Turn() != chess.Black {
		t.Fatal("own event was applied")
	}
}

func TestAgentResign(t *testing.T) {
	a, out := newTestApp(t)
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.resign", Body: json.RawMessage(`{}`)})
	if a.game.Outcome() != chess.WhiteWon || !a.over {
		t.Fatalf("outcome %s, over %v", a.game.Outcome(), a.over)
	}
	if _, ok := lastEmitted(out, "game.over"); !ok {
		t.Fatal("game.over not emitted")
	}
}

func TestHintRenderedAsStatus(t *testing.T) {
	a, _ := newTestApp(t)
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.hint", Body: json.RawMessage(`{"text":"castle early"}`)})
	if a.status != "hint: castle early" {
		t.Fatalf("status: %q", a.status)
	}
}

func TestHumanMoveQueuedWhenNotTurn(t *testing.T) {
	a, _ := newTestApp(t)
	if !a.humanMove("e4") {
		t.Fatal("humanMove ended game")
	}
	// Black to move: human types ahead.
	if !a.humanMove("Nf3") {
		t.Fatal("humanMove ended game")
	}
	if strings.Join(a.sans, " ") != "e4" {
		t.Fatalf("queued move applied early: %v", a.sans)
	}
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"e5"}`)})
	if got := a.sans; len(got) != 3 || got[2] != "Nf3" {
		t.Fatalf("queued move not drained: %v", got)
	}
}

func TestParseMoveForms(t *testing.T) {
	a, _ := newTestApp(t)
	for _, s := range []string{"e4", "e2e4", "Nf3", "g1f3", "  d4  "} {
		if _, err := parseMove(a.game, s); err != nil {
			t.Errorf("parseMove(%q): %v", s, err)
		}
	}
	for _, s := range []string{"e9", "zz", "Ke4", "e2e9"} {
		if _, err := parseMove(a.game, s); err == nil {
			t.Errorf("parseMove(%q) should fail", s)
		}
	}
}

func TestHumanResign(t *testing.T) {
	a, out := newTestApp(t)
	if a.humanMove("resign") {
		t.Fatal("resign should end the app loop")
	}
	if a.game.Outcome() != chess.BlackWon || !a.over {
		t.Fatalf("outcome %s, over %v", a.game.Outcome(), a.over)
	}
	if _, ok := lastEmitted(out, "game.over"); !ok {
		t.Fatal("game.over not emitted")
	}
}

func TestRenderBoardInitial(t *testing.T) {
	a, _ := newTestApp(t)
	b := renderBoard(a.game)
	if !strings.Contains(b, "r   n   b   q   k   b   n   r") || !strings.Contains(b, "R   N   B   Q   K   B   N   R") {
		t.Fatalf("board missing back ranks:\n%s", b)
	}
	if !strings.Contains(b, "8 |") || !strings.Contains(b, "1 |") {
		t.Fatalf("board missing ranks:\n%s", b)
	}
}

func TestCheckFlagInPosition(t *testing.T) {
	a, out := newTestApp(t)
	if !a.humanMove("f3") {
		t.Fatal("early end")
	}
	if !a.humanMove("g4") {
		t.Fatal("early end")
	}
	a.applyInbound(&proxEvent{ID: "a0", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"e5"}`)})
	a.applyInbound(&proxEvent{ID: "a1", Src: "agent:test", Type: "chess.move", Body: json.RawMessage(`{"san":"Qh4#"}`)})
	pos, _ := lastEmitted(out, "game.position")
	if b := evBody(t, pos); b["check"] != true {
		t.Fatalf("check flag missing: %v", b)
	}
}

func TestHintTextForms(t *testing.T) {
	for body, want := range map[string]string{
		`{"text":"a"}`:  "a",
		`{"hint":"b"}`:  "b",
		`"bare string"`: "bare string",
		`{"other":1}`:   "",
	} {
		if got := hintText(json.RawMessage(body)); got != want {
			t.Errorf("hintText(%s) = %q, want %q", body, got, want)
		}
	}
}
