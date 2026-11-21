# chess-demo

Terminal chess: **you play White**, an AI agent plays **Black** over
[proxenos](https://github.com/dshumphr/proxenos) — the app and the agent
coordinate through one append-only NDJSON log. The agent needs no
app-specific code; it reads the stream and replies with events.

Rules/legal-move validation/game-over detection come from
[notnil/chess](https://github.com/notnil/chess) — nothing hand-rolled.

## Build

[proxenos](https://github.com/dshumphr/proxenos) is a prerequisite — the app
shells out to its CLI for every send:

```sh
git clone https://github.com/dshumphr/proxenos && cd proxenos
go build -o ~/.local/bin/proxenos .   # then make sure ~/.local/bin is on PATH

cd ..
git clone https://github.com/dshumphr/chess-demo && cd chess-demo
go build -o chess-demo ./cmd/chess-demo
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

## Play with your agent (omp session — the extension)

Tell your agent in the omp chat: **"let's play chess"**. If the
`proxenos-chess` extension is installed (`omp-extension/proxenos-chess.ts`,
copied to `~/.omp/agent/extensions/`), the agent calls `chess_open`: the app
opens in a Tern block beside the conversation on stream `chess`, and the
extension watches the stream and wakes the agent only when it must act —
a `game.position` whose body says `turn == "black"` (its move), `game.over`,
or an `action.rejected`. Everything else arrives as silent context on its
next turn. The agent replies on the stream, e.g.:

```sh
proxenos send chess chess.move '{"san":"Nf6"}' --as agent:me
```

No paste-prompts, no manual `proxenos tail` polling. See
[`omp-extension/README.md`](omp-extension/README.md).

### Raw-CLI fallback (no extension)

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

## Requirements & limitations

- **Middleware: [proxenos](https://github.com/dshumphr/proxenos)** — the
  append-only stream the app and the agent share. It is a hard prerequisite
  for the agent flow (without it the game still runs fine — the app just
  waits on Black forever, and your terminal moves work as usual).
- **Harness: omp + Tern (closed beta)** — the only harness wired up today,
  via the extension in `omp-extension/`. No other harnesses are wired up yet.

The app itself is harness-agnostic: the **stream contract** (the NDJSON event
vocabulary in `cmd/chess-demo/proxenos-contract.json`, also seeded onto the
stream as a `stream.contract` record) is the only interface. Any harness that
can read a file and run a shell command can play Black — see the raw-CLI
fallback above.

## Agent setup — one prompt

Prerequisites: **omp**, **Tern**, and a machine with Go. Paste this into a
fresh omp session and the agent does the whole setup:

> Set up the chess-demo project for me, end to end:
> 1. Clone https://github.com/dshumphr/proxenos and build it to
>    `~/.local/bin/proxenos` (`go build -o ~/.local/bin/proxenos .`), then
>    verify `proxenos --help` works (add `~/.local/bin` to PATH if not).
> 2. Clone https://github.com/dshumphr/chess-demo, and from the repo root
>    build the app: `go build -o chess-demo ./cmd/chess-demo`.
> 3. Typecheck the omp extension — install `typescript` and
>    `@oh-my-pi/pi-coding-agent` as dev dependencies in the repo (that
>    package supplies the `ExtensionAPI` types), run `tsc --noEmit` on
>    `omp-extension/proxenos-chess.ts`, then copy that file to
>    `~/.omp/agent/extensions/`.
> 4. Verify: `go build ./...` and `go test ./...` pass, and `./script/e2e.sh`
>    runs green (it uses the proxenos binary from step 1; override with
>    `PROXENOS=/path/to/proxenos` if it is not on PATH).
> When everything is green, tell me to start a NEW omp session — extensions
> load at session start, and `/reload-plugins` does not pick up brand-new
> files. In the new session, "let's play chess" starts a game.

## End-to-end test (no LLM)

```sh
./script/e2e.sh
```

Locates the proxenos binary (`$PROXENOS` env override, then `PATH`, then
`~/.local/bin/proxenos`, then `../proxenos/proxenos`) and builds the app,
runs scripted White moves (f3, g4) against a **fake agent** shell loop that
replies with legal Black moves (e5, Qh4# — fool's mate), asserts
`game.over {"result":"0-1"}` lands on the stream and the app exits 0, then
`proxenos rm`s the stream. Zero LLM calls.

## Layout

- `cmd/chess-demo/` — the app: `main.go` (loop/wiring), `app.go` (game
  state + protocol), `ui.go` (ASCII board), embedded contract.
- `internal/prox/` — proxenos client: CLI sends, direct file-poll reader
  with the DESIGN.md reader rules, unit-tested.
- `script/e2e.sh` — scripted human vs. fake-agent end-to-end test, no LLM.
- `omp-extension/` — the omp harness glue (`proxenos-chess.ts`): `chess_open`
  tool, Tern spawn, stream watcher that wakes the agent only on its turn.
