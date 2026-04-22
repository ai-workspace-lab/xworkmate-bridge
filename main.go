package main

import (
	"encoding/json"
	"fmt"
	"os"

	"xworkmate-bridge/internal/acp"
	"xworkmate-bridge/internal/geminiadapter"
	"xworkmate-bridge/internal/hermesadapter"
	"xworkmate-bridge/internal/toolbridge"
)

var (
	buildCommit   = ""
	buildVersion  = "v1.0-beta2"
	buildDate     = ""
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version":
			if err := printBridgeVersionInfo(); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			return
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := acp.Serve(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "acp-stdio" {
		acp.RunStdio(os.Stdin, os.Stdout)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "gemini-acp-adapter" {
		if err := geminiadapter.Serve(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hermes-acp-adapter" {
		if err := hermesadapter.Serve(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}

	toolbridge.Run(os.Stdin, os.Stdout)
}

func printBridgeVersionInfo() error {
	payload := map[string]any{
		"status":  "ok",
		"commit":  buildCommit,
		"version": buildVersion,
		"build-date": buildDate,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(encoded, '\n'))
	return err
}
