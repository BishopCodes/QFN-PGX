package cli

// Tiny ANSI prompt toolkit — the "astro feel" without a TUI dependency: each
// question is one line with a dim default/hint, arrow-free, fully testable.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiAcc   = "\x1b[36m"
	ansiBad   = "\x1b[31m"
	ansiOK    = "\x1b[32m"
)

var stdin = bufio.NewReader(os.Stdin)

func styled(s, ansi string) string { return ansi + s + ansiReset }

// Ask prompts for a string; empty input returns def.
func Ask(label, def, hint string) (string, error) {
	line := fmt.Sprintf("%s %s", styled("? "+label, ansiAcc), styled("("+def+")", ansiDim))
	if hint != "" {
		line += " " + styled(hint, ansiDim)
	}
	fmt.Println(line)
	fmt.Print("  > ")
	txt, err := stdin.ReadString('\n')
	if err != nil {
		return "", err
	}
	txt = strings.TrimSpace(txt)
	if txt == "" {
		return def, nil
	}
	return txt, nil
}

// AskInt wraps Ask with parsing + range validation loop.
func AskInt(label string, def int, min, max int, hint string) (int, error) {
	for {
		s, err := Ask(label, strconv.Itoa(def), hint)
		if err != nil {
			return 0, err
		}
		v, err := strconv.Atoi(s)
		if err != nil || v < min || v > max {
			fmt.Println(styled(fmt.Sprintf("  ! need a number %d..%d", min, max), ansiBad))
			continue
		}
		return v, nil
	}
}

// AskFloat wraps Ask with float parsing.
func AskFloat(label string, def float64, min, max float64, hint string) (float64, error) {
	for {
		s, err := Ask(label, strconv.FormatFloat(def, 'g', -1, 64), hint)
		if err != nil {
			return 0, err
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < min || v > max {
			fmt.Println(styled(fmt.Sprintf("  ! need a number %g..%g", min, max), ansiBad))
			continue
		}
		return v, nil
	}
}

// AskBool is a y/n prompt. Empty input (or re-typing the displayed default)
// returns def.
func AskBool(label string, def bool, hint string) (bool, error) {
	disp := "y/N"
	if def {
		disp = "Y/n"
	}
	for {
		s, err := Ask(label, disp, hint)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", strings.ToLower(disp):
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Println(styled("  ! answer y or n", ansiBad))
	}
}

// AskChoice renders a numbered list.
func AskChoice(label string, opts []string, defIdx int, hint string) (int, error) {
	for {
		var sb strings.Builder
		sb.WriteString(styled("? "+label, ansiAcc) + "\n")
		for i, o := range opts {
			marker := " "
			if i == defIdx {
				marker = "›"
			}
			sb.WriteString(fmt.Sprintf("  %s %d) %s\n", styled(marker, ansiAcc), i+1, o))
		}
		if hint != "" {
			sb.WriteString(styled("  " + hint, ansiDim) + "\n")
		}
		fmt.Print(sb.String())
		fmt.Print("  > ")
		txt, err := stdin.ReadString('\n')
		if err != nil {
			return 0, err
		}
		txt = strings.TrimSpace(txt)
		if txt == "" {
			return defIdx, nil
		}
		n, err := strconv.Atoi(txt)
		if err != nil || n < 1 || n > len(opts) {
			fmt.Println(styled(fmt.Sprintf("  ! pick 1..%d", len(opts)), ansiBad))
			continue
		}
		return n - 1, nil
	}
}

// AskPassword prompts twice (masked) and enforces the minimum length.
func AskPassword(label string, minLen int) (string, error) {
	for {
		a, err := readSecret(label)
		if err != nil {
			return "", err
		}
		if len(a) < minLen {
			fmt.Println(styled(fmt.Sprintf("  ! at least %d characters", minLen), ansiBad))
			continue
		}
		b, err := readSecret(label + " (again)")
		if err != nil {
			return "", err
		}
		if a != b {
			fmt.Println(styled("  ! passwords differ — start over", ansiBad))
			continue
		}
		return a, nil
	}
}

func readSecret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Non-tty (piped): read a plain line — enables scripts/CI.
		fmt.Fprintln(os.Stderr, styled("? "+label+" (stdin, NOT masked)", ansiDim))
		line, err := stdin.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	fmt.Print(styled("? "+label, ansiAcc) + " ")
	b, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", errors.New("empty")
	}
	return string(b), nil
}

func okf(format string, a ...any)  { fmt.Println(styled("  ✓ "+fmt.Sprintf(format, a...), ansiOK)) }
func warnf(format string, a ...any) { fmt.Println(styled("  ! "+fmt.Sprintf(format, a...), "\x1b[33m")) }
func badf(format string, a ...any)  { fmt.Fprintln(os.Stderr, styled("  ✗ "+fmt.Sprintf(format, a...), ansiBad)) }
func dimf(format string, a ...any)  { fmt.Println(styled(fmt.Sprintf(format, a...), ansiDim)) }
