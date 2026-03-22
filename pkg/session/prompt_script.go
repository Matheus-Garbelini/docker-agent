package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// promptScriptTimeout is the maximum time a prompt script is allowed to run.
const promptScriptTimeout = 30 * time.Second

// runPromptScript executes a command string and returns its stdout as a prompt.
// The command is split on whitespace into program and arguments (e.g. "python script.py arg1 arg2").
// The working directory for the command is set to workDir.
func runPromptScript(workDir, command string) (string, error) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "", errors.New("empty prompt script command")
	}

	ctx, cancel := context.WithTimeout(context.Background(), promptScriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = workDir

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("prompt script %q failed: %w", command, err)
	}

	return strings.TrimSpace(string(output)), nil
}
