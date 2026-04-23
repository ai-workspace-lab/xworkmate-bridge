package main

import (
	"encoding/json"
	"fmt"
	"os"

	"xworkmate-bridge/internal/acp"
	"xworkmate-bridge/internal/geminiadapter"
	"xworkmate-bridge/internal/hermesadapter"
	"xworkmate-bridge/internal/opencodeadapter"
)

var (
	buildCommit   = ""
	buildVersion  = "v1.1.0"
	buildDate     = ""
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "serve":
		if err := acp.Serve(args); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "adapter":
		handleAdapterCommand(args)
	case "stdio":
		acp.RunStdio(os.Stdin, os.Stdout)
	case "version", "-v", "--version":
		printBridgeVersionInfo()
	default:
		// Backward compatibility for old subcommands (optional, but we said no backward compatibility)
		// However, for the transition, we can be nice or just fail.
		// The user said "彻底清理陈旧代码", so I'll just fail with a help message.
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func handleAdapterCommand(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: xworkmate-bridge adapter <type> [options]\n")
		fmt.Fprintf(os.Stderr, "Supported types: gemini, hermes, opencode\n")
		os.Exit(1)
	}

	adapterType := args[0]
	adapterArgs := args[1:]

	var err error
	switch adapterType {
	case "gemini":
		err = geminiadapter.Serve(adapterArgs)
	case "hermes":
		err = hermesadapter.Serve(adapterArgs)
	case "opencode":
		err = opencodeadapter.Serve(adapterArgs)
	default:
		fmt.Fprintf(os.Stderr, "Unknown adapter type: %s\n", adapterType)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Adapter error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf("xworkmate-bridge %s\n\n", buildVersion)
	fmt.Println("Usage:")
	fmt.Println("  xworkmate-bridge serve [options]          Start the main ACP bridge server")
	fmt.Println("  xworkmate-bridge adapter <type> [options] Start a specific adapter (gemini, hermes, opencode)")
	fmt.Println("  xworkmate-bridge stdio                     Run the bridge over stdio")
	fmt.Println("  xworkmate-bridge version                   Print version info")
}

func printBridgeVersionInfo() error {
	payload := map[string]any{
		"status":     "ok",
		"commit":     buildCommit,
		"version":    buildVersion,
		"build-date": buildDate,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}
