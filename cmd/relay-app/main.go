// Command relay-app is the Relay desktop application: a native window
// (system webview via Wails v2 — WebView2/WKWebView/WebKitGTK, no bundled
// Chromium) hosting the same embedded UI and Go engine that `relay ui`
// serves. The webview talks to internal/ui's handler directly through the
// Wails asset server, so the desktop app and the browser UI are one
// codebase.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/muhaymien96/relay/internal/engine"
	"github.com/muhaymien96/relay/internal/store"
	"github.com/muhaymien96/relay/internal/ui"
)

func main() {
	workspace := flag.String("workspace", "", "workspace directory (default: $RELAY_WORKSPACE or cwd)")
	flag.Parse()

	root := *workspace
	if root == "" {
		root = os.Getenv("RELAY_WORKSPACE")
	}
	if root == "" {
		if flag.NArg() > 0 {
			root = flag.Arg(0)
		} else {
			root = "."
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-app:", err)
		os.Exit(1)
	}
	db, err := store.Open(filepath.Join(abs, "relay.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-app:", err)
		os.Exit(1)
	}
	defer db.Close()
	if empty, err := db.Empty(); err == nil && empty {
		_, _ = db.SeedFromDir(abs)
	}

	srv := &ui.Server{DB: db, Engine: engine.NewOptions()}
	err = wails.Run(&options.App{
		Title:     "Relay — " + filepath.Base(abs),
		Width:     1280,
		Height:    820,
		MinWidth:  760,
		MinHeight: 480,
		// No embedded asset FS: every request (index and /api/*) routes to
		// the same handler `relay ui` serves over localhost.
		AssetServer: &assetserver.Options{Handler: srv.Handler()},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-app:", err)
		os.Exit(1)
	}
}
