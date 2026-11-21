#!/usr/bin/env bash
# End-to-end test: human-vs-agent chess over a proxenos stream, no LLM.
#
# - locates the proxenos binary ($PROXENOS env override, then PATH, then
#   ~/.local/bin/proxenos, then ../proxenos/proxenos — see
#   https://github.com/dshumphr/proxenos to clone and build it)
# - builds chess-demo
# - runs the app with a scripted White (f3, g4 — heading into fool's mate)
# - runs a "fake agent" shell loop that reads the stream and replies with
#   legal Black moves from a fixed script (e5, Qh4#)
# - asserts the stream reaches game.over {"result":"0-1"} and the app exits 0
# - removes the stream (proxenos rm)
#
# Zero LLM calls: everything is deterministic shell.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PROX=${PROXENOS:-proxenos}
if ! command -v "$PROX" >/dev/null 2>&1; then
  for cand in "$HOME/.local/bin/proxenos" "$ROOT/../proxenos/proxenos"; do
    if [ -x "$cand" ]; then PROX="$cand"; break; fi
  done
fi
if ! command -v "$PROX" >/dev/null 2>&1 && [ ! -x "$PROX" ]; then
  echo "e2e: no proxenos binary found." >&2
  echo "e2e: set PROXENOS=/path/to/proxenos, or clone https://github.com/dshumphr/proxenos" >&2
  echo "e2e: and build it (go build -o ~/.local/bin/proxenos .) so it is on PATH." >&2
  exit 1
fi
case "$PROX" in */*) PROX=$(readlink -f "$PROX") ;; esac
echo "e2e: proxenos binary: $PROX"
WORK=$(mktemp -d)
APP_PID=""
cleanup() {
  [[ -n "$APP_PID" ]] && kill "$APP_PID" 2>/dev/null || true
  [[ -n "${STREAM:-}" && -n "${PROX:-}" ]] && "$PROX" rm "$STREAM" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

export PATH="$WORK/bin:$PATH"
export XDG_DATA_HOME="$WORK/data" # isolate the proxenos streams dir

echo "== building =="
mkdir -p "$WORK/bin"
( cd "$ROOT" && go build -o "$WORK/bin/chess-demo" ./cmd/chess-demo )
STREAM="chess-e2e-$$"

echo "== scripted white moves =="
printf 'f3\ng4\n' > "$WORK/moves.txt"

echo "== fake agent (plays Black from a fixed script) =="
cat > "$WORK/agent.sh" <<'AGENT'
#!/usr/bin/env bash
# Fake agent: reads the stream file directly, plays Black from a fixed
# script (one move per black-to-move position), exits on game.over.
set -euo pipefail
: "${PROX:?}" "${STREAM:?}"
MOVES=("e5" "Qh4#")
i=0
path=""
for _ in $(seq 1 50); do
  path=$("$PROX" ls --json | jq -r --arg s "$STREAM" '.[] | select(.name==$s) | .path')
  [[ -n "$path" ]] && break
  sleep 0.1
done
[[ -n "$path" ]] || { echo "agent: stream file never appeared" >&2; exit 1; }
off=0
while :; do
  size=$(wc -c < "$path")
  if (( size > off )); then
    chunk=$(tail -c +"$((off + 1))" "$path")
    off=$size
    while IFS= read -r line; do
      [[ -z "$line" ]] && continue
      type=$(printf '%s' "$line" | jq -r '.type // empty' 2>/dev/null || true)
      case "$type" in
        game.position)
          turn=$(printf '%s' "$line" | jq -r '.body.turn // empty')
          if [[ "$turn" == "black" && $i -lt ${#MOVES[@]} ]]; then
            echo "agent: playing ${MOVES[$i]}" >&2
            "$PROX" send "$STREAM" chess.move "{\"san\":\"${MOVES[$i]}\"}" --as agent:fake-e2e >/dev/null
            i=$((i + 1))
          fi
          ;;
        game.over)
          echo "agent: saw game.over" >&2
          exit 0
          ;;
      esac
    done <<< "$chunk"
  fi
  sleep 0.1
done
AGENT
chmod +x "$WORK/agent.sh"

echo "== starting app =="
export PROX STREAM
"$WORK/bin/chess-demo" --stream "$STREAM" --moves "$WORK/moves.txt" \
  > "$WORK/app.out" 2> "$WORK/app.err" &
APP_PID=$!

# The agent exits when it sees game.over.
if ! timeout 20 "$WORK/agent.sh"; then
  echo "FAIL: fake agent did not see game.over within 20s" >&2
  tail -5 "$WORK/app.err" "$WORK/app.out" >&2 || true
  exit 1
fi

echo "== waiting for app to exit =="
for _ in $(seq 1 100); do
  stat=$(ps -o stat= -p "$APP_PID" 2>/dev/null || true)
  [[ -z "$stat" || "$stat" == Z* ]] && break
  sleep 0.1
done
if [[ -n "$stat" && "$stat" != Z* ]]; then
  echo "FAIL: app still running after game over" >&2
  exit 1
fi
wait "$APP_PID" || { echo "FAIL: app exited non-zero" >&2; exit 1; }

echo "== asserting stream contents =="
path="$XDG_DATA_HOME/proxenos/streams/$STREAM.jsonl"
grep -q '"type":"stream.contract"' "$path" && echo "ok: stream.contract seeded"
grep -q '"type":"game.started"' "$path" && echo "ok: game.started"
grep -q '"type":"game.position"' "$path" && echo "ok: game.position events present"
grep -q '"san":"Qh4#"' "$path" && echo "ok: agent move Qh4# on stream"
grep -q '"result":"0-1"' "$path" && echo "ok: game.over result 0-1"
if grep -q '"type":"action.rejected"' "$path"; then
  echo "FAIL: unexpected action.rejected on stream:" >&2
  grep '"type":"action.rejected"' "$path" >&2
  exit 1
fi
echo "ok: no action.rejected events"

echo "== stream integrity (proxenos check) =="
"$PROX" check "$STREAM"

echo "== stream event log =="
jq -r '"\(.src)\t\(.type)\t\(.body.san // .body.result // "")"' "$path"

"$PROX" rm "$STREAM"
STREAM="" # cleanup trap: already removed

echo "E2E PASS"
