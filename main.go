// amazon-photos-backup keeps a local copy of an Amazon Photos library.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"amazon-photos-backup/internal/amazon"
	"amazon-photos-backup/internal/daemon"
)

const usage = `Usage: amazon-photos-backup [command]

Commands:
  run           Back up on a schedule, forever (default)
  login         Read browser cookies from standard input (or --file PATH),
                check them with Amazon and save them
  once          Run a single backup and exit
  status        Show the state of the backup
  healthcheck   Exit non-zero when the backup needs attention

Environment:
  BACKUP_DIR            where photos are stored      (default /data/photos)
  STATE_DIR             cookies, index, status       (default /data/state)
  SYNC_INTERVAL         pause between backups        (default 6h)
  DOWNLOAD_CONCURRENCY  parallel downloads, 1-16     (default 4)
  AMAZON_TLD            fr, de, co.uk, com...        (default: from cookies)
  NOTIFY_URL            webhook for alerts (plain-text POST, ntfy-compatible)
  USER_AGENT            browser User-Agent to present
  LOG_LEVEL             debug, info, warn, error     (default info)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	command := "run"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	if command == "help" || command == "-h" || command == "--help" {
		fmt.Print(usage)
		return 0
	}

	cfg, err := daemon.ConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil && os.Getenv("LOG_LEVEL") != "" {
		fmt.Fprintln(os.Stderr, "configuration error: LOG_LEVEL must be debug, info, warn or error")
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if command == "run" || command == "once" || command == "login" {
		if err := daemon.Preflight(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	switch command {
	case "run":
		if err := daemon.Run(ctx, cfg, log); err != nil {
			log.Error("daemon stopped", "err", err)
			return 1
		}
		return 0

	case "once":
		res, err := daemon.Once(ctx, cfg, log)
		if errors.Is(err, daemon.ErrBusy) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err != nil {
			return 1 // already logged
		}
		if res.Failed > 0 {
			return 3
		}
		return 0

	case "login":
		in, err := loginInput(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "login:", err)
			return 2
		}
		if err := daemon.Login(cfg, in, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", err)
			return 1
		}
		return 0

	case "status":
		return printStatus(cfg)

	case "healthcheck":
		if err := daemon.Healthcheck(cfg, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			return 1
		}
		fmt.Println("healthy")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
	return 2
}

// loginInput returns the reader the cookies come from: a file, a pipe, or a
// single line typed (pasted) at a terminal.
func loginInput(args []string) (io.Reader, error) {
	switch {
	case len(args) == 2 && args[0] == "--file":
		data, err := os.ReadFile(args[1])
		if err != nil {
			return nil, err
		}
		return strings.NewReader(string(data)), nil
	case len(args) != 0:
		return nil, errors.New("expected no argument, or --file PATH")
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return os.Stdin, nil // piped
	}
	fmt.Println("Paste the value of the Cookie header from your browser, then press Enter:")
	line, err := bufio.NewReaderSize(os.Stdin, 1<<20).ReadString('\n')
	if err != nil && line == "" {
		return nil, err
	}
	// Terminals cut pasted lines at 4096 bytes, which would silently drop
	// the last cookies.
	if len(line) >= 4095 {
		return nil, errors.New("the pasted line is too long for a terminal and was cut; save it to a file and use: login < file")
	}
	return strings.NewReader(line), nil
}

func printStatus(cfg daemon.Config) int {
	st, err := daemon.LoadStatus(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no status yet (has the backup ever run?):", err)
		return 1
	}
	when := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return fmt.Sprintf("%s (%s ago)", t.Local().Format("2006-01-02 15:04:05"), time.Since(t).Round(time.Second))
	}
	fmt.Printf("State:           %s\n", st.State)
	fmt.Printf("Last run start:  %s\n", when(st.LastRunStart))
	fmt.Printf("Last success:    %s\n", when(st.LastSuccess))
	r := st.LastResult
	fmt.Printf("Last result:     %d remote, %d downloaded (%.1f MB), %d already saved, %d adopted, %d failed, %d unreachable\n",
		r.Remote, r.Downloaded, float64(r.Bytes)/1e6, r.Skipped, r.Adopted, r.Failed, r.Unreachable)
	if r.Mismatched > 0 {
		fmt.Printf("                 %d saved although Amazon's checksum disagrees with the bytes it serves (see logs)\n", r.Mismatched)
	}
	if st.LastError != "" {
		fmt.Printf("Last error:      %s\n", st.LastError)
	}
	store := amazon.NewStore(cfg.StateDir + "/cookies.json")
	if err := store.Load(); err != nil {
		fmt.Println("Session:         no cookies saved; run the login command")
	} else {
		fmt.Printf("Session:         %d cookies saved\n", store.Len())
	}
	if st.State == daemon.StateAuthRequired {
		fmt.Println("\nAction needed: run the login command with fresh cookies from your browser.")
	}
	return 0
}
