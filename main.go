// Package main implements coder-logstream-incus, a daemon that streams
// Incus VM console and cloud-init logs to the Coder agent startup logs API.
package main

import (
	"fmt"
	"net/url"
	"os"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/serpent"

	// Never remove this. Certificates are not bundled as part
	// of the binary, so this is necessary for all TLS connections.
	_ "github.com/breml/rootcerts"
)

func main() {
	cmd := root()
	err := cmd.Invoke().WithOS().Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func root() *serpent.Command {
	var (
		coderURL     string
		incusSocket  string
		pollInterval string
		project      string
	)

	cmd := &serpent.Command{
		Use:   "coder-logstream-incus",
		Short: "Stream Incus VM console and cloud-init logs to Coder startup logs.",
		Options: serpent.OptionSet{
			{
				Name:          "coder-url",
				Flag:          "coder-url",
				FlagShorthand: "u",
				Env:           "CODER_URL",
				Value:         serpent.StringOf(&coderURL),
				Description:   "URL of the Coder instance.",
			},
			{
				Name:          "socket",
				Flag:          "socket",
				FlagShorthand: "s",
				Env:           "INCUS_SOCKET",
				Default:       "",
				Value:         serpent.StringOf(&incusSocket),
				Description:   "Path to the Incus Unix socket. Leave empty to use the default.",
			},
			{
				Name:          "poll-interval",
				Flag:          "poll-interval",
				FlagShorthand: "i",
				Env:           "CODER_INCUS_POLL_INTERVAL",
				Default:       "5s",
				Value:         serpent.StringOf(&pollInterval),
				Description:   "How often to poll for new Incus instances.",
			},
			{
				Name:          "project",
				Flag:          "project",
				FlagShorthand: "p",
				Env:           "INCUS_PROJECT",
				Default:       "default",
				Value:         serpent.StringOf(&project),
				Description:   "Incus project to watch for instances.",
			},
		},
		Handler: func(inv *serpent.Invocation) error {
			if coderURL == "" {
				return fmt.Errorf("--coder-url is required")
			}
			parsedURL, err := url.Parse(coderURL)
			if err != nil {
				return fmt.Errorf("parse coder URL: %w", err)
			}
			if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
				return fmt.Errorf("CODER_URL must include http:// or https:// scheme, got: %q", coderURL)
			}

			logger := slog.Make(sloghuman.Sink(inv.Stderr)).Leveled(slog.LevelDebug)

			streamer, err := newIncusLogStreamer(inv.Context(), incusLogStreamerOptions{
				coderURL:    parsedURL,
				socketPath:  incusSocket,
				project:     project,
				logger:      logger,
			})
			if err != nil {
				return fmt.Errorf("create incus log streamer: %w", err)
			}
			defer streamer.Close()

			select {
			case err := <-streamer.errChan:
				return fmt.Errorf("incus log streamer: %w", err)
			case <-inv.Context().Done():
			}
			return nil
		},
	}

	return cmd
}
