# chess-demo

Terminal chess: **you play White**, an AI agent plays **Black** over
[proxenos](../proxenos) — the app and the agent coordinate through one
append-only NDJSON log. The agent needs no app-specific code; it reads the
stream and replies with events.

Rules/legal-move validation/game-over detection come from
[notnil/chess](https://github.com/notnil/chess) — nothing hand-rolled.

## Build

```sh
go build -o chess-demo ./cmd/chess-demo   # requires the proxenos binary on PATH
go test ./...
```

## 60-second local demo (two terminals)

Terminal 1 — start the app (it creates the stream idempotently on first run):

```sh
./chess-demo
```

Terminal 2 — watch the stream:

```sh
proxenos tail chess -f --pretty
```

Type `e4` in the app; terminal 2 shows `game.started`, `game.move`, and a
`game.position` with the FEN, turn, check flag, and an ASCII board. Until an
agent attaches, the app just waits on Black (`Black is thinking…`).

Moves: SAN (`Nf3`, `e4`, `e8=Q`) or from/to (`e2e4`), or `resign`.

## Contract (excerpt)

The app seeds the stream with a `stream.contract` record teaching agents the
vocabulary:

```json
{"type":"stream.contract","src":"app:chess","body":{
  "name":"chess",
  "emits": {
    "game.started":   {"fen":"rnbq…", "humanColor":"white"},
    "game.position":  {"fen":"…", "lastMove":"e4", "turn":"black", "check":false, "ascii":"…"},
    "game.move":      {"from":"e2","to":"e4","san":"e4","by":"white"},
    "game.over":      {"result":"0-1","reason":"checkmate"},
    "action.rejected":{"action":"chess.move","reason":"not Black's turn — White to move"}
  },
  "accepts": {
    "chess.move":   {"from":"e7","to":"e5"},
    "chess.resign": {},
    "chess.hint":   {"text":"watch the a2-g8 diagonal before castling"}
  },
  "instructions":"…play Black; move when game.position says turn=black…"
}}
```

Full record: [`cmd/chess-demo/proxenos-contract.json`](cmd/chess-demo/proxenos-contract.json).
The app polls the stream file directly (DESIGN.md reader rules: skip empty /
malformed lines, hold torn prefixes, re-baseline on replacement) and dedupes
on event `id`. Illegal or out-of-turn agent moves are answered with
`action.rejected` — never a silent drop.

## Play against your agent (omp session)

Start the app (`./chess-demo`, or `./chess-demo --stream mygame`), then paste
this to the agent in your omp session:

> Attach to the proxenos stream `chess` (contract is on the stream as a
> `stream.contract` record — read it with
> `proxenos tail chess --tail 1 --pretty`). You are playing **Black** in a
> live terminal chess game against me; I play White and enter moves in the
> app. Watch the stream (`proxenos tail chess -f` or poll the file): every
> `game.position` with `"turn":"black"` is your move — reply with exactly one
> `chess.move` event, e.g.
> `proxenos send chess chess.move '{"san":"Nf6"}' --as agent:me`
> (or `{"from":"e7","to":"e5"}`; for promotions use san). If your move comes
> back as `action.rejected`, read the reason and send a corrected move. You
> can surface a note on my screen with
> `proxenos send chess chess.hint '{"text":"…"}' --as agent:me` and concede
> with `proxenos send chess chess.resign '{}' --as agent:me`.
> Then make your first move: the initial `game.position` says turn=black
> only after I move, so wait for it.

The app applies your `chess.move` events as if they were typed, announces
every applied move (both sides) with `game.move` + `game.position`, and
emits `game.over` with the result and reason (checkmate, resignation,
stalemate, …) when the library calls it.

## End-to-end test (no LLM)

```sh
./script/e2e.sh
```

Builds proxenos (from `$PROXENOS_SRC`, default `/tmp/proxenos`) and the app,
runs scripted White moves (f3, g4) against a **fake agent** shell loop that
replies with legal Black moves (e5, Qh4# — fool's mate), asserts
`game.over {"result":"0-1"}` lands on the stream and the app exits 0, then
`proxenos rm`s the stream.

## Layout

- `cmd/chess-demo/` — the app: `main.go` (loop/wiring), `app.go` (game
  state + protocol), `ui.go` (ASCII board), embedded contract.
- `internal/prox/` — proxenos client: CLI sends, direct file-poll reader
  with the DESIGN.md reader rules, unit-tested.
