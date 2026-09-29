package main

import (
	"io"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/term"
	"golang.org/x/text/width"
)

var terminalEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)

func runeColumns(value rune) int {
	if unicode.IsControl(value) || unicode.Is(unicode.Mn, value) || unicode.Is(unicode.Me, value) || value == '\u200d' {
		return 0
	}
	switch width.LookupRune(value).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}

func displayWidth(value string) int {
	count := 0
	for _, character := range terminalEscape.ReplaceAllString(value, "") {
		count += runeColumns(character)
	}
	return count
}

func padDisplay(value string, columns int) string {
	return value + strings.Repeat(" ", max(0, columns-displayWidth(value)))
}

func fitDisplay(value string, columns int) string {
	value = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(terminalEscape.ReplaceAllString(value, ""))
	if columns <= 0 {
		return ""
	}
	if displayWidth(value) <= columns {
		return value
	}
	var result strings.Builder
	used := 0
	for _, character := range value {
		used += runeColumns(character)
		if used > columns-1 {
			break
		}
		result.WriteRune(character)
	}
	return result.String() + "…"
}

func outputColumns(state *outputContext, writer io.Writer) int {
	if state != nil && state.columns > 0 {
		return state.columns
	}
	if file, ok := writerFile(writer); ok {
		if columns, _, err := term.GetSize(int(file.Fd())); err == nil && columns >= 20 {
			return columns
		}
	}
	return 80
}
