# chess-demo ↔ omp extension

This is the **harness-side glue** for the conversation-first flow. It is what
makes the user just say **"let's play chess"** in their omp chat: the agent
calls `chess_open`, and the app opens in a Tern block beside the conversation,
wired to the same proxenos stream. It is a single TypeScript extension
(`proxenos-chess.ts`) that omp loads; chess-demo itself only speaks the
proxenos stream, so it stays usable without any agent.

Adapted from bibtui's proven `proxenos-bibtui.ts` pattern.

## What it does

1. **Registers `chess_open`** — the tool the agent calls when the user asks to
   play. Params: `dir` (where chess-demo lives), `stream` (default `chess`),
   `launch` (`auto` | `none` | `detached`), `block` (Tern pane to split
   beside), and an optional `wake` override.
2. **Spawns chess-demo in a Tern block** beside the conversation:
   `tern split <pane> right --cwd <dir> --keep-open -- ./chess-demo --stream <stream>`.
3. **Watches the stream file** (leading-newline framing, cursor idempotency,
   digest buffer — the proxenos reader rules). You play **Black**; the human
   plays White and types moves in the app.
4. **Wakes the agent only when it must act** — this is the body filter, not a
   type filter:

   | Event | Wake? |
   |---|---|
   | `game.position` with `body.turn == "black"` | **WAKE** — your move |
   | `game.over` | **WAKE** — report the result |
   | `action.rejected` | **WAKE** — fix the move |
   | `game.started`, `game.move`, `chess.hint`, out-of-turn `game.position` | silent context (hidden, next turn) |

   The body filter matters: the app emits `game.position` after **both**
   sides move, so a type-only filter would wake the agent on its own echo.
5. **Teaches the agent the reply command** in the tool result:
   `proxenos send <stream> chess.move '{"san":"Nf6"}' --as agent:me`
   (or `{"from":"e7","to":"e5"}`; promotions prefer san), plus `chess.hint`
   and `chess.resign`.
6. **Cleans up** on session shutdown: closes the Tern block it created and
   stops its watcher. It never kills an app the user launched themselves.

## Install

```sh
cp omp-extension/proxenos-chess.ts ~/.omp/agent/extensions/
```

Then start a **new** omp session — extensions load at session start; a
brand-new file is not picked up by `/reload-plugins` (verified against omp
18.22.2).

Requirements: `proxenos` and `tern` on `PATH`; chess-demo built at `dir`
(defaults to the session's cwd).

## Play

Tell your agent: **"let's play chess"** (or "play chess with me"). That's it —
the agent opens the app beside the conversation, you type `e4` in the app, and
its reply appears on the board.

## Notes

- `launch:'none'` only *watches* an already-running app. `launch:'detached'`
  runs the app headless in its own pty (watcher works; nobody can type).
- The `wake` override changes the body filter: `{turn: "white"}` flips sides;
  `{over: false, rejected: false}` trims wakes to the bare minimum.
- The extension is deliberately chess-demo-specific: it knows the binary name
  and the move vocabulary. A general extension would read the app's contract
  instead; that is a future step, not this file.
