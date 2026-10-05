// Command embolt is a proxy between Emby apps and one Emby server that sends
// each request through the mihomo node best suited to it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Set by -ldflags at release.
var (
	version = "dev"
	commit  = "unknown"
)

const usage = `usage: embolt <command> [flags]

commands:
  serve        run the proxy and the pane
  replay       run the model over logged samples and report its calibration
  healthcheck  exit 0 if the local pane answers /healthz
  version      print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "replay":
		err = replay(args)
	case "healthcheck":
		err = healthcheck(args)
	case "version":
		fmt.Printf("embolt %s (%s)\n", version, commit)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "embolt:", err)
		os.Exit(1)
	}
}

// healthcheck lets a distroless image, which has no shell or curl, check itself.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9090", "pane address")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
