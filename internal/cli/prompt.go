package cli

// Tiny ANSI prompt toolkit — the "astro feel" without a TUI dependency: each
// question is one line with a dim default/hint; choice prompts add arrow-key
// selection on a tty, but everything stays non-tty-safe and testable.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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

	escSaveCur = "\x1b7"   // save cursor position
	escRestCur = "\x1b8"   // restore cursor position
	eraseLine  = "\x1b[2K" // erase whole line
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

// AskChoice renders a numbered list. On a terminal it is an interactive
// picker: ↑/↓ (or j/k, Home/End, PgUp/PgDn) move, 1-9 jump, Enter picks,
// Ctrl-C aborts. On a non-tty (pipes, CI, smoke scripts) it keeps the exact
// legacy line behavior: type the number, empty input takes the default.
func AskChoice(label string, opts []string, defIdx int, hint string) (int, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return askChoiceInteractive(label, opts, defIdx, hint)
	}
	return askChoiceLine(label, opts, defIdx, hint)
}

// askChoiceLine is the legacy line-based path, preserved verbatim for
// non-tty input so piped/CI flows keep working.
func askChoiceLine(label string, opts []string, defIdx int, hint string) (int, error) {
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
		idx, ok := parseChoiceIndex(txt, len(opts), defIdx)
		if !ok {
			fmt.Println(styled(fmt.Sprintf("  ! pick 1..%d", len(opts)), ansiBad))
			continue
		}
		return idx, nil
	}
}

// parseChoiceIndex maps a typed line to an option index. Empty input takes
// defIdx; a number outside 1..n (or junk) reports ok=false.
func parseChoiceIndex(txt string, n, defIdx int) (int, bool) {
	txt = strings.TrimSpace(txt)
	if txt == "" {
		return defIdx, true
	}
	v, err := strconv.Atoi(txt)
	if err != nil || v < 1 || v > n {
		return 0, false
	}
	return v - 1, true
}

func clampIdx(v, n int) int {
	if v < 0 {
		return 0
	}
	if v >= n {
		return n - 1
	}
	return v
}

// askChoiceInteractive is the raw-mode picker. The first frame saves the
// cursor; later frames restore to that anchor and repaint each line in place
// (no scroll-back, no flicker). Option text is assumed short enough not to
// wrap. Raw mode disables output post-processing, so all newlines written
// while raw are explicitly "\r\n".
func askChoiceInteractive(label string, opts []string, defIdx int, hint string) (int, error) {
	if len(opts) == 0 {
		return 0, errors.New("no options to choose from")
	}
	fd := int(os.Stdin.Fd())
	cur := clampIdx(defIdx, len(opts))
	raw, err := term.MakeRaw(fd)
	if err != nil {
		// Can't enter raw mode — degrade to the old line prompt.
		return askChoiceLine(label, opts, cur, hint)
	}
	defer func() { _ = term.Restore(fd, raw) }()

	status, first := "", true
	for {
		drawChoiceFrame(label, opts, cur, hint, status, first)
		first, status = false, ""

		b, err := stdin.ReadByte()
		if err != nil {
			fmt.Print("\r\n")
			return 0, err
		}
		switch {
		case b == 3: // ctrl-c → abort
			fmt.Print("\r\n")
			return 0, errors.New("cancelled")
		case b == 4: // ctrl-d → like a closed pipe
			fmt.Print("\r\n")
			return 0, io.EOF
		case b == '\r' || b == '\n': // enter → pick
			picked := fmt.Sprintf("%d) %s", cur+1, opts[cur])
			fmt.Print("\r" + eraseLine + "  > " + styled(picked, ansiDim) + "\r\n")
			return cur, nil
		case b == 27: // ESC-prefixed sequence (arrows, home/end, pgup/pgdn)
			seq, err := readEscapeSeq()
			if err != nil {
				fmt.Print("\r\n")
				return 0, err
			}
			switch seq {
			case "A": // ↑
				cur = clampIdx(cur-1, len(opts))
			case "B": // ↓
				cur = clampIdx(cur+1, len(opts))
			case "H", "5~": // Home / PgUp
				cur = 0
			case "F", "6~": // End / PgDn
				cur = len(opts) - 1
			}
		case b == 'k':
			cur = clampIdx(cur-1, len(opts))
		case b == 'j':
			cur = clampIdx(cur+1, len(opts))
		case b >= '1' && b <= '9': // number jump shortcut
			if n := int(b - '0'); n <= len(opts) {
				cur = n - 1
			} else {
				status = styled(fmt.Sprintf("! pick 1..%d", len(opts)), ansiBad)
			}
		}
	}
}

// readEscapeSeq consumes the tail of an escape sequence after the initial
// ESC and returns its discriminator: "A"/"B" (↑/↓, CSI or SS3 form),
// "H"/"F" (Home/End) or "5~"/"6~" (PgUp/PgDn). Unknown tails yield "".
func readEscapeSeq() (string, error) {
	b, err := stdin.ReadByte()
	if err != nil {
		return "", err
	}
	if b != '[' && b != 'O' {
		return "", nil // ESC + <char>: ignore.
	}
	if b == 'O' { // SS3 (arrows in application mode): one byte follows.
		b, err := stdin.ReadByte()
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err = stdin.ReadByte() // CSI final byte
	if err != nil {
		return "", err
	}
	if b == '5' || b == '6' { // CSI 5~ / 6~ — eat the trailing '~'.
		if _, err := stdin.ReadByte(); err != nil {
			return "", err
		}
		return string([]byte{b, '~'}), nil
	}
	return string(b), nil
}

// drawChoiceFrame paints (or repaints) the choice block: label, the option
// list with the "›" marker on cur, dim hint, then a prompt line carrying an
// optional inline status (errors land there so the block never grows lines).
func drawChoiceFrame(label string, opts []string, cur int, hint, status string, first bool) {
	var sb strings.Builder
	if first {
		sb.WriteString(escSaveCur)
	} else {
		sb.WriteString(escRestCur)
	}
	sb.WriteString(eraseLine + styled("? "+label, ansiAcc) + "\r\n")
	for i, o := range opts {
		marker, text := " ", o
		if i == cur {
			marker, text = "›", styled(o, ansiAcc)
		}
		sb.WriteString(fmt.Sprintf("%s  %s %d) ", eraseLine, styled(marker, ansiAcc), i+1) + text + "\r\n")
	}
	if hint != "" {
		sb.WriteString(eraseLine + styled("  "+hint, ansiDim) + "\r\n")
	}
	sb.WriteString(eraseLine + "  > " + status)
	fmt.Print(sb.String())
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
