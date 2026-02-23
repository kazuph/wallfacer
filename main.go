package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"changkun.de/wallfacer/internal/logger"
)

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: wallfacer <command> [arguments]\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  run          start the Kanban server\n")
	fmt.Fprintf(os.Stderr, "  env          show configuration and env file status\n")
	fmt.Fprintf(os.Stderr, "\nRun 'wallfacer <command> -help' for more information on a command.\n")
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Fatal(logger.Main, "home dir", "error", err)
	}
	configDir := filepath.Join(home, ".wallfacer")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "env":
		runEnvCheck(configDir)
	case "run":
		runServer(configDir, os.Args[2:])
	case "-help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "wallfacer: unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runEnvCheck(configDir string) {
	envFile := envOrDefault("ENV_FILE", filepath.Join(configDir, ".env"))

	fmt.Printf("Config directory:  %s\n", configDir)
	fmt.Printf("Data directory:    %s\n", envOrDefault("DATA_DIR", filepath.Join(configDir, "data")))
	fmt.Printf("Env file:          %s\n", envFile)
	fmt.Printf("Container command: %s\n", envOrDefault("CONTAINER_CMD", "docker"))
	fmt.Println()

	if info, err := os.Stat(configDir); err != nil {
		fmt.Printf("[!] Config directory does not exist (run 'wallfacer run' to auto-create)\n")
	} else if !info.IsDir() {
		fmt.Printf("[!] %s is not a directory\n", configDir)
	} else {
		fmt.Printf("[ok] Config directory exists\n")
	}

	raw, err := os.ReadFile(envFile)
	if err != nil {
		fmt.Printf("[!] Env file not found: %s\n", envFile)
		fmt.Printf("    Run 'wallfacer run' once to auto-create a template, then set your token.\n")
		return
	}
	fmt.Printf("[ok] Env file exists\n")

	vals := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	// Authentication: at least one token must be set.
	oauthToken := vals["CLAUDE_CODE_OAUTH_TOKEN"]
	apiKey := vals["ANTHROPIC_API_KEY"]
	switch {
	case oauthToken != "" && oauthToken != "your-oauth-token-here":
		masked := oauthToken[:4] + "..." + oauthToken[len(oauthToken)-4:]
		if len(oauthToken) <= 8 {
			masked = strings.Repeat("*", len(oauthToken))
		}
		fmt.Printf("[ok] CLAUDE_CODE_OAUTH_TOKEN is set (%s)\n", masked)
	case apiKey != "":
		masked := apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
		if len(apiKey) <= 8 {
			masked = strings.Repeat("*", len(apiKey))
		}
		fmt.Printf("[ok] ANTHROPIC_API_KEY is set (%s)\n", masked)
	default:
		fmt.Printf("[!] No API token found in %s — set CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY\n", envFile)
	}

	if v := vals["ANTHROPIC_BASE_URL"]; v != "" {
		fmt.Printf("[ok] ANTHROPIC_BASE_URL = %s\n", v)
	} else {
		fmt.Printf("[ ] ANTHROPIC_BASE_URL not set (using default)\n")
	}
	if v := vals["CLAUDE_CODE_MODEL"]; v != "" {
		fmt.Printf("[ok] CLAUDE_CODE_MODEL = %s\n", v)
	} else {
		fmt.Printf("[ ] CLAUDE_CODE_MODEL not set (using Claude Code default)\n")
	}

	containerCmd := envOrDefault("CONTAINER_CMD", "docker")
	if _, err := exec.LookPath(containerCmd); err != nil {
		fmt.Printf("[!] Container runtime not found: %s\n", containerCmd)
	} else {
		fmt.Printf("[ok] Container runtime found: %s\n", containerCmd)

		// Check docker sandbox availability.
		out, err := exec.Command(containerCmd, "sandbox", "ls", "--json").Output()
		if err != nil {
			fmt.Printf("[!] Docker sandbox not available (%s sandbox ls failed: %v)\n", containerCmd, err)
			fmt.Printf("    Ensure Docker Desktop with sandbox support is installed.\n")
		} else {
			fmt.Printf("[ok] Docker sandbox available (output: %s)\n", strings.TrimSpace(string(out)))
		}
	}
}

func initConfigDir(configDir, envFile string) {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		logger.Fatal(logger.Main, "create config dir", "error", err)
	}

	if _, err := os.Stat(envFile); os.IsNotExist(err) {
		content := "# Authentication: set ONE of the two token variables below.\n" +
			"CLAUDE_CODE_OAUTH_TOKEN=your-oauth-token-here\n" +
			"# ANTHROPIC_API_KEY=sk-ant-...\n\n" +
			"# Optional: custom Anthropic-compatible API base URL.\n" +
			"# ANTHROPIC_BASE_URL=https://api.anthropic.com\n\n" +
			"# Optional: override the model used by Claude Code (e.g. claude-opus-4-5).\n" +
			"# CLAUDE_CODE_MODEL=\n"
		if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
			logger.Fatal(logger.Main, "create env file", "error", err)
		}
		logger.Main.Info("created env file — edit it and set your token", "path", envFile)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	default:
		return
	}
	exec.Command(cmd, url).Start()
}
