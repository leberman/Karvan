package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The Karvan camel, for terminals only (never in hook output, which Claude
// reads, or in background runs). Its eye shows the mood.
// The art, without its two text lines; text starts at column textCol.
var camelArt = []string{
	`             [#][=]`,
	`     ,_     _/~~~~~\_`,
	`    (E )___/         \__`,
	`     \_       ~~~~      )`,
	`       \_______________/`,
	`        ||  ||    ||  ||`,
}

const textCol = 30

type mood int

const (
	moodHappy mood = iota
	moodWorried
)

// camel renders the camel with a speech line and a smaller line under it
// (the version); color adds ANSI colors.
func camel(m mood, say, sub string, color bool) string {
	eye := "^"
	if m == moodWorried {
		eye = "o"
	}
	const (
		tan    = "\033[38;5;180m"
		cargo  = "\033[38;5;221m"
		saddle = "\033[38;5;30m"
		bold   = "\033[1m"
		dim    = "\033[2m"
		warn   = "\033[38;5;209m"
		reset  = "\033[0m"
	)
	var out []string
	for i, l := range camelArt {
		l = strings.Replace(l, "E", eye, 1)
		text := ""
		switch i {
		case 2:
			text = say
		case 3:
			text = sub
		}
		pad := ""
		if text != "" {
			pad = strings.Repeat(" ", textCol-len(l))
		}
		if !color {
			out = append(out, l+pad+text)
			continue
		}
		switch i {
		case 0:
			l = strings.Replace(l, "[#][=]", cargo+"[#][=]"+reset, 1)
		case 1:
			l = tan + strings.Replace(l, "~~~~~", saddle+"~~~~~"+tan, 1) + reset
		default:
			l = tan + l + reset
		}
		switch i {
		case 2:
			c := bold
			if m == moodWorried {
				c = bold + warn
			}
			text = c + text + reset
		case 3:
			text = dim + text + reset
		}
		out = append(out, l+pad+text)
	}
	return strings.Join(out, "\n")
}

// showCamel prints the camel if w is the terminal the user is looking at.
func (a *App) showCamel(w io.Writer, m mood, say string) {
	if !a.Terminal || w != a.Stdout {
		return
	}
	fmt.Fprintln(w, camel(m, say, "Karvan "+a.Build.short(), os.Getenv("NO_COLOR") == ""))
	fmt.Fprintln(w)
}
