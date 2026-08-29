// Command aistation is the single entry point every platform's start script
// launches: it wires up hardware detection, the model registry/watcher, the
// engine bootstrap approval flow, and the local HTTP server, then opens the
// browser. One binary, one command, nothing else to run first.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"aistation/internal/engine"
	"aistation/internal/safego"
	"aistation/internal/server"
)

func main() {
	dir := flag.String("dir", ".", "path to the shared portable folder (models/, data/, engines/)")
	port := flag.Int("port", 2222, "port to listen on (0 = pick any free port)")
	noLAN := flag.Bool("no-lan", false, "bind to localhost only, don't expose on the local network")
	noBrowser := flag.Bool("no-browser", false, "don't auto-open the default browser")
	flag.Parse()

	absDir, err := absPath(*dir)
	if err != nil {
		log.Fatalf("resolving --dir: %v", err)
	}

	app, err := server.NewApp(absDir)
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	stopWatch := make(chan struct{})
	safego.Go(func() { app.StartWatcher(stopWatch) })
	go func() { <-ctx.Done(); close(stopWatch) }()

	stopUsage := make(chan struct{})
	safego.Go(func() { app.StartUsageSampler(stopUsage) })
	go func() { <-ctx.Done(); close(stopUsage) }()

	safego.Go(func() { runApprovalPrompts(ctx, app) })

	actualPort, err := app.Serve(ctx, *port, !*noLAN)
	if err != nil {
		log.Fatalf("failed to start server: %v", err)
	}

	localURL := fmt.Sprintf("http://127.0.0.1:%d", actualPort)
	fmt.Println()
	fmt.Println("  AI Workstation is running.")
	fmt.Println("  Local:  " + localURL)
	if !*noLAN {
		if lan := server.LANURL(actualPort); lan != "" {
			fmt.Println("  LAN:    " + lan + "   (scan the QR code in the UI's \"Phone access\" button)")
		}
	}
	fmt.Println("  Press Ctrl+C to stop.")
	fmt.Println()

	if !*noBrowser {
		openBrowser(localURL)
	}

	<-ctx.Done()
	fmt.Println("\nShutting down...")
	time.Sleep(300 * time.Millisecond) // let in-flight requests and the HTTP server's own shutdown drain
}

func absPath(p string) (string, error) {
	if p == "" {
		p = "."
	}
	return filepath.Abs(p)
}

// runApprovalPrompts watches engine bootstrap state and, the first time
// either component needs approval, asks once on stdin — a terminal fallback
// for the same approval the web UI's "Approve" button triggers, so headless
// or not-yet-opened-browser runs still work.
func runApprovalPrompts(ctx context.Context, app *server.App) {
	askedText, askedImage := false, false
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			text, image := app.Engines().Snapshot()
			if text.Status == engine.StatusAwaitingApproval && !askedText {
				askedText = true
				safego.Go(func() { promptYesNo("text engine", text.Message, app.Engines().ApproveText) })
			}
			if image.Status == engine.StatusAwaitingApproval && !askedImage {
				askedImage = true
				safego.Go(func() { promptYesNo("image engine", image.Message, app.Engines().ApproveImage) })
			}
		}
	}
}

func promptYesNo(label, message string, approve func()) {
	fmt.Printf("\n[%s] %s\n", label, message)
	fmt.Print("Approve download? Type 'y' and press Enter (or approve from the web UI instead): ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		approve()
	} else {
		fmt.Println("Not approved from terminal — you can still approve from the web UI at any time.")
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default: // linux, android/termux
		if _, err := exec.LookPath("termux-open-url"); err == nil {
			cmd = exec.Command("termux-open-url", url)
		} else if _, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command("xdg-open", url)
		}
	}
	if cmd == nil {
		return
	}
	_ = cmd.Start()
}
