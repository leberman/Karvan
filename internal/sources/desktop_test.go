package sources

import (
	"strings"
	"testing"
)

func TestSwapPaths(t *testing.T) {
	doc := `{"cwd":"/home/u/work/app","originCwd":"/home/u/work/app/sub","other":"/home/u/work/app-old","n":1,` +
		`"title":"fix \"auth\"","list":["/home/u/work/app/x",{"deep":"/home/u/work/app"}],"lastActivityAt":1763300000123}`
	got, err := swapPaths([]byte(doc), "/home/u/work/app", Placeholder, "linux")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cwd":"{{karvan:project}}","originCwd":"{{karvan:project}}/sub","other":"/home/u/work/app-old","n":1,` +
		`"title":"fix \"auth\"","list":["{{karvan:project}}/x",{"deep":"{{karvan:project}}"}],"lastActivityAt":1763300000123}`
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	back, _ := swapPaths(got, Placeholder, "/home/v/temp/app", "linux")
	if strings.Contains(string(back), "karvan") || !strings.Contains(string(back), `"originCwd":"/home/v/temp/app/sub"`) {
		t.Fatalf("%s", back)
	}
	// Windows paths are matched case-insensitively and with escaped "\".
	win := `{"cwd":"C:\\Users\\U\\App\\src","lastActivityAt":1}`
	got, _ = swapPaths([]byte(win), `c:\users\u\app`, Placeholder, "windows")
	if !strings.Contains(string(got), `"cwd":"{{karvan:project}}\\src"`) {
		t.Fatalf("%s", got)
	}
}

func TestIsDesktopApp(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/usr/lib/claude-desktop/electron /usr/lib/claude-desktop/app.asar": true,
		"/opt/Claude/Claude --type=renderer":                                true,
		"claude-desktop":                                                    true,
		"/home/u/.local/bin/claude --resume":                                false,
		"claude":                                                            false,
		"/usr/bin/node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js": false,
		"vim notes-about-claude-desktop.md":                                    false,
	} {
		if got := isDesktopApp(cmd); got != want {
			t.Errorf("isDesktopApp(%q) = %v", cmd, got)
		}
	}
}
