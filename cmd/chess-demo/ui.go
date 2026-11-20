package main

import (
	"fmt"
	"strings"

	"github.com/notnil/chess"
)

// renderBoard draws a plain-ASCII board from the top side's view: white
// pieces uppercase, black lowercase, files a–h left to right.
func renderBoard(g *chess.Game) string {
	sqmap := g.Position().Board().SquareMap()
	var b strings.Builder
	b.WriteString("  +------------------------+\n")
	for rank := 7; rank >= 0; rank-- {
		fmt.Fprintf(&b, "%d |", rank+1)
		for file := range 8 {
			p, ok := sqmap[chess.Square(rank*8+file)]
			if !ok {
				b.WriteString("   .")
				continue
			}
			fmt.Fprintf(&b, "   %c", pieceChar(p))
		}
		fmt.Fprintf(&b, "  | %d\n", rank+1)
	}
	b.WriteString("  +------------------------+\n")
	b.WriteString("    a   b   c   d   e   f   g   h\n")
	return b.String()
}

func pieceChar(p chess.Piece) byte {
	var c byte
	switch p.Type() {
	case chess.King:
		c = 'K'
	case chess.Queen:
		c = 'Q'
	case chess.Rook:
		c = 'R'
	case chess.Bishop:
		c = 'B'
	case chess.Knight:
		c = 'N'
	case chess.Pawn:
		c = 'P'
	}
	if p.Color() == chess.Black {
		c += 'a' - 'A'
	}
	return c
}

// renderScreen draws the full one-screen state.
func (a *app) renderScreen() {
	fmt.Print("\033[2J\033[H") // clear screen, home cursor
	fmt.Println("chess-demo — you are White (proxenos stream:", a.client.Stream+")")
	fmt.Println()
	fmt.Print(renderBoard(a.game))
	fmt.Println()
	if n := len(a.sans); n > 0 {
		fmt.Println("moves:", strings.Join(a.sans, " "))
	}
	out := a.game.Outcome()
	if out != chess.NoOutcome {
		fmt.Printf("game over: %s (%s)\n", out, strings.ToLower(a.game.Method().String()))
		return
	}
	turn := a.game.Position().Turn()
	if turn == chess.White {
		fmt.Print("your move (SAN like Nf3, or e2e4; 'resign' quits): ")
	} else {
		fmt.Println("Black is thinking… (agent plays via the stream)")
	}
	if a.status != "" {
		fmt.Println()
		fmt.Println(a.status)
		a.status = ""
	}
}

// renderFinal prints the end state without clearing (for scripted runs).
func (a *app) renderFinal() {
	fmt.Println()
	fmt.Print(renderBoard(a.game))
	out := a.game.Outcome()
	if out != chess.NoOutcome {
		fmt.Printf("game over: %s (%s)\n", out, strings.ToLower(a.game.Method().String()))
	}
	if n := len(a.sans); n > 0 {
		fmt.Println("moves:", strings.Join(a.sans, " "))
	}
}
