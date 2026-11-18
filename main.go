// OverusTeX is a small LaTeX editor that runs in a browser tab: it serves
// its page on 127.0.0.1 and builds with latexmk or pdflatex.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"
)

func main() {
	noBrowser := flag.Bool("no-browser", false, "print the address but do not open a browser")
	port := flag.Int("port", 0, "port on 127.0.0.1 (0 picks a free one)")
	idle := flag.Duration("idle", time.Minute, "quit when no OverusTeX tab has been open this long")
	flag.Parse()

	s, err := newServer(*port, *idle)
	if err != nil {
		fmt.Fprintln(os.Stderr, "overustex:", err)
		os.Exit(1)
	}
	fmt.Println("OverusTeX is running at", s.URL())
	fmt.Println("It stops when you press Quit, or when no OverusTeX tab has been open for", *idle)
	if !*noBrowser {
		cmd := browserCommand(s.URL())
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "overustex: could not open a browser:", err)
		} else {
			go cmd.Wait()
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := s.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "overustex:", err)
		os.Exit(1)
	}
}
