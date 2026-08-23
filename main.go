package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

const usage = `mainstreet - a launcher and viewer for tmux sessions

usage:
  mainstreet                 open the TUI
  mainstreet state [--json]  print sessions, windows and worktrees
  mainstreet help            show this message
`

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mainstreet: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		return runTUI()
	}
	switch args[0] {
	case "state":
		return cmdState(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func cmdState(args []string) error {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print state as JSON on stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := gather(context.Background(), tmuxClient{r: execRunner{}})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	return printState(os.Stdout, st)
}
