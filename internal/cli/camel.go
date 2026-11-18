package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The Karvan camel, for terminals only (never in hook output, which Claude
// reads, or in background runs). Its eye shows the mood.
const camelArt = `             [#][=]
     ,_     _/~~~~~\_
    (E )___/         \__     SAY
     \_       ~~~~      )
       \_______________/
        ||  ||    ||  ||`

type mood int

const (
	moodHappy mood = iota
	moodWorried
)

// camel renders the camel with a speech line; color adds ANSI colors.
func camel(m mood, say string, color bool) string {
	eye := "^"
	if m == moodWorried {
		eye = "o"
	}
	art := strings.Replace(camelArt, "E", eye, 1)
	if !color {
		return strings.Replace(art, "SAY", say, 1)
	}
	const (
		tan    = "\033[38;5;180m"
		cargo  = "\033[38;5;221m"
		saddle = "\033[38;5;30m"
		bold   = "\033[1m"
		warn   = "\033[38;5;209m"
		reset  = "\033[0m"
	)
	sayColor := bold
	if m == moodWorried {
		sayColor = bold + warn
	}
	lines := strings.Split(art, "\n")
	lines[0] = strings.Replace(lines[0], "[#][=]", cargo+"[#][=]"+reset, 1)
	lines[1] = tan + strings.Replace(lines[1], "~~~~~", saddle+"~~~~~"+tan, 1) + reset
	lines[2] = tan + strings.Replace(lines[2], "SAY", reset+sayColor+say+reset, 1) + reset
	for i := 3; i < len(lines); i++ {
		lines[i] = tan + lines[i] + reset
	}
	return strings.Join(lines, "\n")
}

// showCamel prints the camel if w is the terminal the user is looking at.
func (a *App) showCamel(w io.Writer, m mood, say string) {
	if !a.Terminal || w != a.Stdout {
		return
	}
	fmt.Fprintln(w, camel(m, say, os.Getenv("NO_COLOR") == ""))
	fmt.Fprintln(w)
}
