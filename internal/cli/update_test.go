package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestUpdate(t *testing.T) {
	app, _, _ := newApp(t)
	app.Build.Version = "0.1.1"
	latest := "v0.1.1"
	app.UpdateCheck = func(bool) (string, error) { return latest, nil }
	ran := ""
	app.Installer = func(_ context.Context, tag, _ string, _ io.Writer) error { ran = tag; return nil }

	if out := run(t, app, "update"); !strings.Contains(out, "is the latest version") || ran != "" {
		t.Fatalf("%s", out)
	}
	if out := run(t, app, "status"); strings.Contains(out, "update:") {
		t.Fatalf("notice without a new release:\n%s", out)
	}
	latest = "v0.2.0"
	if out := run(t, app, "status"); !strings.Contains(out, "update:       Karvan v0.2.0 is available (you have 0.1.1): run `karvan update`") {
		t.Fatalf("%s", out)
	}
	if out := run(t, app, "update", "--check"); !strings.Contains(out, "v0.2.0 is available") || ran != "" {
		t.Fatalf("%s", out)
	}
	if out := run(t, app, "update"); !strings.Contains(out, "Updating Karvan 0.1.1 -> v0.2.0") || ran != "v0.2.0" {
		t.Fatalf("%s", out)
	}
	latest = ""
	if code := Run(app, []string{"update"}); code != ExitError {
		t.Fatal("offline update must fail clearly")
	}
}
