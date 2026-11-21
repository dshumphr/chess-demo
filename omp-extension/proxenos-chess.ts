/**
 * proxenos-chess — omp extension for the conversation-first chess flow.
 *
 * The user tells their agent "let's play chess"; the agent calls chess_open
 * and this extension launches ./chess-demo in a Tern split beside the
 * conversation, watches the proxenos stream file, and injects the events the
 * agent must ACT on into the live session (pi.sendUserMessage). Everything
 * else arrives as silent hidden context on the agent's next turn.
 *
 * Wake policy (filter by event BODY, not just type — the app emits
 * game.position after BOTH sides move, so a type-only filter would wake the
 * agent on its own echo):
 *   - game.position  with body.turn === "black"  → WAKE (your move)
 *   - game.over                                  → WAKE (report the result)
 *   - action.rejected                            → WAKE (fix the move)
 *   - game.started / game.move / out-of-turn game.position / chess.hint /
 *     stream.contract / stream.meta / app.started → silent context
 *
 * Reply vocabulary taught to the agent:
 *   proxenos send <stream> chess.move '{"san":"Nf6"}' --as agent:me
 *
 * The app stays harness-agnostic: this file is the only omp-specific glue.
 *
 * Install: copy to ~/.omp/agent/extensions/ (extensions load at session
 * start — a brand-new file is not picked up by /reload-plugins), or run
 * `omp -e /path/to/proxenos-chess.ts`.
 */
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const execFileP = promisify(execFile);

interface Envelope {
	v: number;
	id: string;
	ts: string;
	src: string;
	type: string;
	body: unknown;
	reply_to?: string;
}

interface ChessBody {
	fen?: string;
	humanColor?: string;
	lastMove?: string;
	turn?: string;
	check?: boolean;
	ascii?: string;
	result?: string;
	reason?: string;
	action?: string;
	by?: string;
	san?: string;
}

interface ChessParams {
	dir?: string;
	stream?: string;
	launch?: "auto" | "none" | "detached";
	block?: string;
	wake?: { turn?: string; over?: boolean; rejected?: boolean };
}

interface WakeConfig {
	/** Body turn value on game.position that means "your move". */
	turn: string;
	over: boolean;
	rejected: boolean;
}

interface Watcher {
	stream: string;
	filePath: string;
	offset: number;
	pendingCarry: string;
	timer: NodeJS.Timeout;
	seen: Set<string>;
	spawnedPid?: number;
	/** tern block id when the app was spawned via the tern CLI (shutdown kills it). */
	ternBlockId?: string | null;
	cfg: WakeConfig;
	/** Context lines awaiting the next turn (hidden, zero-cost delivery). */
	digest: string[];
	digestTimer: NodeJS.Timeout | undefined;
}

const POLL_MS = 500;
const STREAM_WAIT_MS = 10_000;
const DIGEST_FLUSH_MS = 2_000;
const DIGEST_MAX_LINES = 24;
const DIGEST_LINE_MAX = 140;
const APP_BIN = "chess-demo";
const DEFAULT_STREAM = "chess";

/**
 * Multiplexer omp can spawn into, detected from the session environment.
 * null = none recognized — the launch chain then tries the tern CLI before
 * falling back to a detached pty.
 */
function detectMultiplexer(): "herdr" | "tmux" | "zellij" | "cmux" | "orca" | null {
	const e = process.env;
	if (e.HERDR_ENV) return "herdr";
	if (e.TMUX) return "tmux";
	if (e.ZELLIJ) return "zellij";
	if (e.CMUX_WORKSPACE_ID || e.CMUX_SURFACE_ID) return "cmux";
	if (e.ORCA_PANE_KEY) return "orca";
	return null;
}

/**
 * Spawn argv in a new Tern block and return the block id. `tern split PANE
 * right --cwd DIR --keep-open -- ARGV...` puts the command in a block beside
 * the calling pane. argv goes straight to execFile: no shell, no quoting.
 * Failures fall through to a new tab, then to the detached-pty path.
 */
async function spawnInTern(args: string[], cwd: string, target?: string): Promise<string | null> {
	// The extension runs in omp's shared kernel, which has no TERN_PANE, so the
	// agent passes its own pane id (from `echo $TERN_PANE`); @focused is the
	// last resort.
	const pane = target || process.env.TERN_PANE || "@focused";
	try {
		const { stdout } = await execFileP(
			"tern",
			["split", "--json", pane, "right", "--cwd", cwd, "--keep-open", "--", ...args],
			{ timeout: 20_000 },
		);
		const created = JSON.parse(stdout) as { block?: number | string };
		if (created?.block !== undefined) return String(created.block);
	} catch (err) {
		console.error(`[proxenos-chess] tern split failed, trying a new tab: ${String(err)}`);
	}
	try {
		const { stdout } = await execFileP(
			"tern",
			["new", "tab", "--json", "--cwd", cwd, "--keep-open", "--", ...args],
			{ timeout: 20_000 },
		);
		const created = JSON.parse(stdout) as { block?: number | string };
		return created?.block !== undefined ? String(created.block) : null;
	} catch (err) {
		console.error(`[proxenos-chess] tern spawn failed: ${String(err)}`);
		return null;
	}
}

/**
 * Resolve the app directory: it must contain an executable `chess-demo`
 * file. A common mistake is passing the repo's PARENT — accept that shape
 * too by looking one level down. Returns null when neither shape holds, so
 * callers can fail loudly instead of spawning `./chess-demo` into
 * "Is a directory" (exit 126).
 */
function resolveAppDir(dir: string): string | null {
	const isExec = (p: string): boolean => {
		try {
			const st = fs.statSync(p);
			return st.isFile() && (st.mode & 0o111) !== 0;
		} catch {
			return false;
		}
	};
	if (isExec(path.join(dir, APP_BIN))) return dir;
	const nested = path.join(dir, APP_BIN);
	if (isExec(nested)) return path.dirname(nested);
	return null;
}

/** Is the app instance this watcher launched still alive? */
async function appAlive(w: Watcher): Promise<boolean> {
	if (w.spawnedPid) {
		try {
			process.kill(w.spawnedPid, 0);
			return true;
		} catch {
			return false;
		}
	}
	if (w.ternBlockId) {
		try {
			const { stdout } = await execFileP("tern", ["ls", "--json"], { timeout: 5000 });
			return stdout.includes(w.ternBlockId);
		} catch {
			return false;
		}
	}
	return false;
}

function sleep(ms: number): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setTimeout(resolve, ms);
	return promise;
}

function streamsDir(): string {
	return path.join(
		process.env.XDG_DATA_HOME ?? path.join(os.homedir(), ".local", "share"),
		"proxenos",
		"streams",
	);
}

export default function proxenosChessExtension(pi: ExtensionAPI) {
	const z = pi.zod;
	const watchers = new Map<string, Watcher>();

	/** Compact one-line summary for the silent-context digest. */
	function eventLine(e: Envelope): string {
		const body = JSON.stringify(e.body ?? {});
		return `${e.type} ${body.length > DIGEST_LINE_MAX ? body.slice(0, DIGEST_LINE_MAX) + "…" : body}`;
	}

	/** Free tier: queue a context line, flushed (debounced) as hidden nextTurn context. */
	function queueDigest(w: Watcher, line: string): void {
		w.digest.push(line);
		if (w.digest.length > DIGEST_MAX_LINES) {
			w.digest.splice(0, w.digest.length - DIGEST_MAX_LINES);
		}
		if (w.digestTimer) return;
		w.digestTimer = setTimeout(() => {
			w.digestTimer = undefined;
			if (w.digest.length === 0) return;
			const lines = w.digest.splice(0, w.digest.length).join("\n");
			pi.sendMessage(
				`[chess ${w.stream}] recent app activity (context only — no reply needed unless the user asks):\n${lines}`,
				{ deliverAs: "nextTurn" },
			);
		}, DIGEST_FLUSH_MS);
	}

	/**
	 * The body filter: return the wake message when this event must start a
	 * turn, null when it is silent context. game.position fires after BOTH
	 * sides move — only body.turn === cfg.turn means it is our move.
	 */
	function wakeMessage(w: Watcher, e: Envelope): string | null {
		const b = (e.body ?? {}) as ChessBody;
		const reply = `proxenos send ${w.stream} chess.move '{"san":"Nf6"}' --as agent:me   (or {"from":"e7","to":"e5"}; promotions prefer san)`;
		switch (e.type) {
			case "game.position":
				if (b.turn === w.cfg.turn) {
					const board = b.ascii ? `${b.ascii}\n` : "";
					return (
						`[chess ${w.stream}] Your move (${w.cfg.turn})` +
						(b.lastMove ? ` — White played ${b.lastMove}` : "") +
						(b.check ? " — CHECK" : "") + `.\n${board}` +
						(b.fen ? `FEN: ${b.fen}\n` : "") +
						`Reply with exactly one move on the stream:\n  ${reply}\n` +
						`If it comes back as action.rejected, read the reason and send a corrected move. ` +
						`Then wait for the next game.position — do not move twice. ` +
						`The user asks questions in THIS chat, not in the app.`
					);
				}
				return null;
			case "game.over":
				if (!w.cfg.over) return null;
				return (
					`[chess ${w.stream}] Game over: ${b.result ?? "?"}` +
					(b.reason ? ` (${b.reason})` : "") +
					`. Tell the user here in chat how it went; offer a rematch ` +
					`(the app must be relaunched to start a new game).`
				);
			case "action.rejected":
				if (!w.cfg.rejected) return null;
				return (
					`[chess ${w.stream}] The app rejected ${b.action ?? "your action"}: ${b.reason ?? "?"}. ` +
					`Read the reason, fix the move, and resend:\n  ${reply}`
				);
			default:
				return null;
		}
	}

	function handleRecord(w: Watcher, raw: string): void {
		const line = raw.trim();
		if (line === "") return;
		let e: Envelope;
		try {
			e = JSON.parse(line) as Envelope;
		} catch {
			return; // torn tail — the offset logic keeps the bytes for next poll
		}
		if (!e || typeof e.type !== "string") return;
		if (w.seen.has(e.id)) return;
		w.seen.add(e.id);
		if (w.seen.size > 1024) {
			const it = w.seen.values();
			for (let i = 0; i < 256; i++) {
				const v = it.next().value;
				if (v === undefined) break;
				w.seen.delete(v);
			}
		}
		// The agent's own sends echo onto the stream; never react to those.
		if (e.src.startsWith("agent:")) return;

		const wake = wakeMessage(w, e);
		if (wake !== null) {
			pi.logger.debug("px-chess: wake", { stream: w.stream, type: e.type, id: e.id });
			pi.sendUserMessage(wake);
			return;
		}
		// Everything else: free tier. No turn, hidden context on the next one.
		queueDigest(w, eventLine(e));
	}

	function startWatcher(stream: string, filePath: string, spawnedPid: number | undefined, cfg: WakeConfig, startOffset: number): Watcher {
		const existing = watchers.get(stream);
		if (existing) {
			// Reconfigure in place — a second chess_open call is a settings
			// update, never a duplicate watcher or a second launch.
			existing.cfg = cfg;
			return existing;
		}
		const w: Watcher = {
			stream,
			filePath,
			offset: startOffset,
			pendingCarry: "",
			timer: null as never,
			seen: new Set(),
			spawnedPid,
			cfg,
			digest: [],
			digestTimer: undefined,
		};
		watchers.set(stream, w);

		w.timer = setInterval(() => {
			let fd: number;
			try {
				fd = fs.openSync(filePath, "r");
			} catch {
				return; // not created yet / transient
			}
			try {
				const size = fs.fstatSync(fd).size;
				if (size < w.offset) {
					// replaced or truncated (compact / new generation): re-baseline at end
					w.offset = size;
					w.pendingCarry = "";
					return;
				}
				if (size === w.offset) return;
				const buf = Buffer.alloc(size - w.offset);
				fs.readSync(fd, buf, 0, buf.length, w.offset);
				const chunk = w.pendingCarry + buf.toString("utf8");
				w.pendingCarry = "";
				// leading-\n framing: records are \n-separated; the file never ends
				// with a newline in steady state, so the final segment may be the
				// newest complete record (EOF rule) or a torn prefix.
				let start = 0;
				for (;;) {
					const nl = chunk.indexOf("\n", start);
					if (nl === -1) break;
					handleRecord(w, chunk.slice(start, nl));
					start = nl + 1;
				}
				const tail = chunk.slice(start);
				if (tail === "") {
					w.offset = size;
					return;
				}
				try {
					JSON.parse(tail); // complete record, unterminated — deliverable now
					handleRecord(w, tail);
					w.offset = size;
				} catch {
					// torn prefix: keep offset at the tail start so nothing is skipped
					w.pendingCarry = tail;
					w.offset = size - Buffer.byteLength(tail);
				}
			} finally {
				fs.closeSync(fd);
			}
		}, POLL_MS);
		return w;
	}

	pi.registerTool({
		name: "chess_open",
		label: "Open chess-demo",
		description:
			"Launch the chess-demo terminal chess app attached to a shared proxenos event stream, " +
			"then watch the stream so you can play. Call this when the user says things like " +
			"\"let's play chess\" or \"play a game of chess with me\". You play BLACK; the human " +
			"plays White and types moves in the app. The app has no chat box: the user talks to " +
			"you here, in this session. " +
			"WAKE POLICY: you are woken ONLY when you must act — a game.position whose body says " +
			'turn:"black" (your move), game.over, or an action.rejected. The app emits ' +
			"game.position after BOTH sides move, so without the body filter you would wake on " +
			"your own echo. Everything else (game.started, game.move, hints) arrives silently as " +
			"hidden context on your next turn. " +
			"Use launch:'none' only if the user explicitly wants to run the app themselves — " +
			"the watcher still works; catch up on state with `proxenos tail <stream> --tail 40 --pretty`. " +
			"Calling this again reconfigures wake settings and relaunches if the app died — " +
			"never improvise tmux/screen commands for this.",
		approval: "exec",
		parameters: z.object({
			dir: z.string().optional().describe("Directory containing the chess-demo binary (default: current directory)"),
			stream: z.string().optional().describe(`Proxenos stream name (default: ${DEFAULT_STREAM}). Use a throwaway name for tests.`),
			launch: z.enum(["auto", "none", "detached"]).optional().describe("'auto' (default) spawns the app in a detected multiplexer pane; 'detached' runs it headless in its own pty; 'none' only watches the stream"),
			block: z.string().optional().describe("Tern block to split beside (the pane you are talking in). Get it from your shell: echo $TERN_PANE."),
			wake: z.object({
				turn: z.string().optional().describe('game.position body.turn value that means your move (default: "black")'),
				over: z.boolean().optional().describe("Wake on game.over (default true)"),
				rejected: z.boolean().optional().describe("Wake on action.rejected (default true)"),
			}).optional().describe("Override the wake policy"),
		}),
		async execute(_toolCallId, params: ChessParams, _signal, _onUpdate, ctx) {
			const requestedDir = params.dir ?? process.cwd();
			const appDir = resolveAppDir(requestedDir);
			const dir = appDir ?? requestedDir;
			const stream = params.stream ?? DEFAULT_STREAM;
			const cfg = params.wake
				? { turn: params.wake.turn ?? "black", over: params.wake.over ?? true, rejected: params.wake.rejected ?? true }
				: { turn: "black", over: true, rejected: true };
			const args = ["./" + APP_BIN, "--stream", stream];

			// Snapshot before launching: the watcher starts at this offset so
			// everything the fresh app instance emits is seen, while stale events
			// from a previous game on the same stream are not replayed.
			const preSize = (() => {
				try {
					return fs.statSync(path.join(streamsDir(), `${stream}.jsonl`)).size;
				} catch {
					return 0;
				}
			})();

			// Reconfigure vs relaunch: an existing watcher is updated in place, but
			// "reconfigure" must never stand in for "the app is open". If the
			// caller wants the app open (launch:'auto') and the previously
			// launched instance is dead, relaunch against the same watcher.
			const existing = watchers.get(stream);
			if (existing) {
				const wantsLaunch = (params.launch ?? "auto") === "auto";
				const alive = await appAlive(existing);
				if (!wantsLaunch || alive) {
					const state = alive
						? `chess-demo is running (pid ${existing.spawnedPid ?? "?"}${existing.ternBlockId ? `, tern block ${existing.ternBlockId}` : ""})`
						: "watch-only mode — the user runs chess-demo themselves";
					startWatcher(stream, "", undefined, cfg, preSize);
					return {
						content: [
							{
								type: "text",
								text:
									`Watcher for stream "${stream}" reconfigured: ` +
									`wake=position(turn=${cfg.turn})/over=${cfg.over}/rejected=${cfg.rejected}. ${state}. ` +
									"No new app instance was launched.",
							},
						],
						details: { stream, reconfigured: true, appAlive: alive },
					};
				}
				// Dead app, caller wants it open: clean stale handles and relaunch.
				existing.spawnedPid = undefined;
				existing.ternBlockId = null;
			}

			let launchedVia: string | null = null;
			let spawnedPid: number | undefined;
			let ternBlockId: string | null = null;
			const launchMode = params.launch ?? "auto";
			if (launchMode !== "none") {
				// Validate before creating any block/session — a wrong dir would
				// spawn a doomed block ("Is a directory", exit 126).
				if (!appDir) {
					return {
						content: [
							{
								type: "text",
								text: `No executable chess-demo in ${requestedDir} (nor ${path.join(requestedDir, APP_BIN)}). ` +
									`Pass dir=<repo root containing the chess-demo binary>, e.g. dir=/path/to/chess-demo. ` +
									`Nothing was launched.`,
							},
						],
						details: { stream, launched: false, dir: requestedDir },
					};
				}
				if (launchMode === "auto") {
					const mux = detectMultiplexer();
					if (mux && ctx.ui?.openTerminal) {
						try {
							const req = { multiplexer: mux, placement: "pane", command: args, cwd: dir } as Parameters<
								NonNullable<typeof ctx.ui.openTerminal>
							>[0];
							await ctx.ui.openTerminal(req);
							launchedVia = `${mux} pane`;
						} catch (err) {
							pi.logger.debug("proxenos-chess: openTerminal failed, falling back", { err: String(err) });
						}
					}
					if (!launchedVia) {
						ternBlockId = await spawnInTern(args, dir, params.block);
						if (ternBlockId !== null) launchedVia = "tern block";
					}
				}
				if (!launchedVia) {
					// Headless: the app gets its own pty via script(1) so the TUI
					// doesn't die on a closed stdout. Output goes nowhere — fine for
					// headless play; the watcher still works.
					const child = spawn("script", ["-qec", args.join(" "), "/dev/null"], {
						cwd: dir,
						detached: true,
						stdio: "ignore",
					});
					child.unref();
					spawnedPid = child.pid;
					launchedVia = "detached pty";
				}
			}

			const filePath = path.join(streamsDir(), `${stream}.jsonl`);
			let found = false;
			for (let i = 0; i < STREAM_WAIT_MS / 250; i++) {
				if (fs.existsSync(filePath) && (launchMode === "none" || fs.statSync(filePath).size > preSize)) {
					found = true;
					break;
				}
				await sleep(250);
			}
			if (!found) {
				const userCmd = `cd ${dir} && ./${APP_BIN} --stream ${stream}`;
				return {
					content: [
						{
							type: "text",
							text: launchMode !== "none"
								? `chess-demo was launched but stream "${stream}" never appeared at ${filePath}. ` +
									`Check that ./${APP_BIN} exists in ${dir} and proxenos is on PATH.`
								: `No stream "${stream}" yet. Ask the user to run: ${userCmd} ` +
									`(in their own split/pane), then call chess_open again with launch:'none'.`,
						},
					],
					details: { stream, launched: false },
				};
			}

			// Start at the pre-launch size so the fresh instance's opening events
			// (game.started, first position) are all seen, while stale events from
			// a previous game on the same stream are not replayed.
			const w = startWatcher(stream, filePath, spawnedPid, cfg, preSize);
			w.ternBlockId = ternBlockId;

			const opening = launchedVia
				? `chess-demo is open (${launchedVia}) on stream "${stream}".` +
					(launchedVia === "tern block"
						? ` It is a split beside this conversation in the same tern tab` +
							(ternBlockId ? ` (block ${ternBlockId}; close it with \`tern close ${ternBlockId}\`).` : ".")
						: "")
				: `Watching stream "${stream}" — waiting for the user to open chess-demo in their own pane (${args.join(" ")}).`;
			const configLine =
				`Wake rules: game.position with body.turn=="${cfg.turn}" (your move), game.over` +
				(cfg.rejected ? ", action.rejected" : "") +
				". Everything else (game.started, game.move, hints, out-of-turn positions) arrives as silent context on your next turn (no reply needed).";
			return {
				content: [
					{
						type: "text",
						text:
							opening + "\n" + configLine + "\n" +
							"You play BLACK; the human plays White and types moves in the app. " +
							"To move, send exactly one event on the stream:\n" +
							`  proxenos send ${stream} chess.move '{"san":"Nf6"}' --as agent:me\n` +
							`  (or {"from":"e7","to":"e5"}; for promotions prefer san) — then WAIT for the next ` +
							`game.position with turn=="${cfg.turn}". Rejected moves come back as action.rejected; fix and resend.\n` +
							`Surface a note on the user's screen: proxenos send ${stream} chess.hint '{"text":"…"}' --as agent:me\n` +
							`Concede: proxenos send ${stream} chess.resign '{}' --as agent:me\n` +
							`Full vocabulary: proxenos tail ${stream} --tail 20 --pretty`,
					},
				],
				details: { stream, launchedVia, filePath },
			};
		},
	});

	pi.on("session_shutdown", async () => {
		for (const w of watchers.values()) {
			clearInterval(w.timer);
			clearTimeout(w.digestTimer);
			if (w.spawnedPid) {
				try {
					process.kill(-w.spawnedPid, "SIGTERM"); // script(1) made it a group leader
				} catch {
					/* already gone */
				}
			}
			if (w.ternBlockId) {
				// Only kill blocks the extension spawned, never user-created ones.
				try {
					await execFileP("tern", ["close", w.ternBlockId], { timeout: 5000 });
				} catch {
					/* already gone or tern absent */
				}
			}
		}
		watchers.clear();
	});
}
