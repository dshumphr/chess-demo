// Command chess-demo is a terminal chess app: the human plays White in the
// terminal, an agent plays Black over a proxenos stream.
//
// Integration follows proxenos/docs/app-integration.md: outbound events go
// through the proxenos CLI (`send`), inbound events are consumed by polling
// the stream file directly with the DESIGN.md reader rules. The proxenos
// contract is embedded and seeded idempotently on startup.
package main

import (
	"bufio"

	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "embed"

	"chess-demo/internal/prox"
)

//go:embed proxenos-contract.json
var contractJSON []byte

const (
	appSrc        = "app:chess"
	pollInterval  = 250 * time.Millisecond
	defaultStream = "chess"
)

func main() {
	stream := flag.String("stream", defaultStream, "proxenos stream name")
	movesFile := flag.String("moves", "", "read human moves from this file instead of stdin (one per line; for scripted runs)")
	flag.Parse()
	run(*stream, *movesFile)
}

// proxLogf wires the integration error sink to a plain log file: the app
// owns the terminal, stderr would corrupt the display (same approach as
// bibtui).
func proxLogf() {
	f, err := os.OpenFile("chess-demo-proxenos.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	proxLog = func(format string, args ...any) {
		fmt.Fprintf(f, time.Now().UTC().Format("15:04:05 ")+format+"\n", args...)
	}
}

func run(stream, movesFile string) {
	proxLogf()

	bin, err := prox.FindBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chess-demo:", err)
		os.Exit(1)
	}
	client := &prox.Client{Bin: bin, Src: appSrc, Stream: stream}
	if err := client.EnsureStream(contractJSON); err != nil {
		fmt.Fprintln(os.Stderr, "chess-demo:", err)
		os.Exit(1)
	}
	path, err := client.ResolveStreamPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chess-demo:", err)
		os.Exit(1)
	}
	reader, err := prox.NewReader(path, proxLog)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chess-demo:", err)
		os.Exit(1)
	}

	a := newApp(client)
	a.emitStarted()
	a.emitPosition("")
	a.renderScreen()

	// Inbound poll goroutine: raw records → channel → main loop. All state
	// mutation stays on this goroutine.
	inbound := make(chan []byte, 64)
	stop := make(chan struct{})
	defer close(stop)
	go reader.PollLoop(inbound, pollInterval, stop)

	// Human input: stdin or a scripted moves file, one move per line.
	input := make(chan string)
	go func() {
		var f *os.File = os.Stdin
		if movesFile != "" {
			file, err := os.Open(movesFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "chess-demo:", err)
				os.Exit(1)
			}
			defer file.Close()
			f = file
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			input <- line
		}
	}()

	for {
		select {
		case rec := <-inbound:
			ev, err := decodeRecord(rec)
			if err != nil {
				proxLog("skipping undecodable record: %v", err)
				continue
			}
			a.applyInbound(ev)
		case line, ok := <-input:
			if !ok {
				// Scripted input exhausted (or stdin closed): wait for the
				// agent to finish the game.
				input = nil
				continue
			}
			if !a.humanMove(line) {
				a.renderFinal()
				return
			}
		}
		a.renderScreen()
		if a.over {
			a.renderFinal()
			return
		}
	}
}
