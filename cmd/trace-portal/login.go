package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/Urenzu/trace-portal/internal/identity"
)

// The subcommands that manage this installation's enrollment.
//
// They are subcommands rather than flags on the server because they are
// interactive and they exit: `trace-portal login` waits for a person to approve
// in a browser and then stops. Folding that into the long-running server would
// mean a flag that sometimes makes the process terminate.

const loginUsage = `trace-portal login — connect this machine to a trace-portal server

usage:
  trace-portal login -server https://app.example.com

Prints a code, waits for you to approve it in a browser, and stores the
resulting credential in the data directory. Nothing is uploaded until the
collector is pointed at the server.
`

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, loginUsage, "\nflags:\n"); fs.PrintDefaults() }

	var (
		server  = fs.String("server", "", "trace-portal server to connect to, e.g. https://app.example.com")
		dataDir = fs.String("data", defaultDataDir(), "directory holding this installation's enrollment")
		label   = fs.String("label", defaultMachineLabel(), "name shown on the approval page")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *server == "" {
		fs.Usage()
		return fmt.Errorf("-server is required")
	}

	// Ctrl-C during the wait should stop cleanly rather than leave a half-run
	// flow printing into a dead terminal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	enrolled, err := identity.Login(ctx, identity.LoginOptions{
		Server:       *server,
		DataDir:      *dataDir,
		MachineLabel: *label,
		Out:          os.Stderr,
		OpenBrowser:  identity.OpenInBrowser,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nConnected. This machine now reports as user %s in tenant %s.\n",
		enrolled.UserID, enrolled.TenantID)
	fmt.Fprintf(os.Stderr, "Turns captured from here carry that identity; everything already in the\n"+
		"archive keeps the identity it was captured with.\n")
	return nil
}

const logoutUsage = `trace-portal logout — disconnect this machine from its server

usage:
  trace-portal logout

Forgets the stored credential and returns to local-only capture. Nothing is
deleted: the archive keeps every turn, with the identity each was captured
under.
`

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, logoutUsage, "\nflags:\n"); fs.PrintDefaults() }
	dataDir := fs.String("data", defaultDataDir(), "directory holding this installation's enrollment")
	if err := fs.Parse(args); err != nil {
		return err
	}

	before, err := identity.Load(*dataDir)
	if err != nil {
		return err
	}
	if before.Local() {
		fmt.Fprintln(os.Stderr, "Not signed in; capture is already local only.")
		return nil
	}
	if _, err := identity.Logout(*dataDir); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Signed out. Capture is local only; the archive is untouched.")
	return nil
}

func runWhoami(args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	dataDir := fs.String("data", defaultDataDir(), "directory holding this installation's enrollment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := identity.Load(*dataDir)
	if err != nil {
		return err
	}
	if e.Local() {
		fmt.Printf("local only — machine %s\n", e.MachineID)
		return nil
	}
	fmt.Printf("user %s in tenant %s on %s — machine %s\n", e.UserID, e.TenantID, e.Server, e.MachineID)
	return nil
}

// defaultMachineLabel is what the approval page shows. The hostname is the one
// thing a person reliably recognises about their own machine, and unlike the
// machine id it never reaches the archive — it is shown once, on a page that
// person is already looking at, so they can tell which laptop they are about to
// connect.
func defaultMachineLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "this machine"
	}
	return host
}

// version identifies this build to a server, so a bad collector release can be
// spotted from the receiving end rather than by asking users what they run. It
// is a constant until there is a release process to stamp it.
const version = "dev"
