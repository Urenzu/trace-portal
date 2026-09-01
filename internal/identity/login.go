package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Login is the terminal half of the device authorization flow.
//
// It asks the server for a code pair, tells the person where to go, and polls
// until they approve. It never speaks to the identity provider, never opens a
// listening socket, and never reads a password — which is what makes it work
// over SSH, in a container, and on a machine with no browser at all, where the
// person reads the code off one screen and types it on another.
//
// The result is written into the same enrollment file an unenrolled install
// already keeps, so everything downstream — the store's stamping, the archive,
// the API — is unchanged by whether anyone has signed in.

// LoginOptions configures one sign-in attempt.
type LoginOptions struct {
	// Server is the trace-portal server to enrol with, e.g.
	// https://app.example.com.
	Server string

	// DataDir holds the enrollment being upgraded. The machine id is read from
	// it and reused, so signing in again from the same install does not make it
	// look like a different machine.
	DataDir string

	// MachineLabel is shown on the approval page. A hostname is the useful
	// thing here: the person approving has to recognise which machine they are
	// connecting.
	MachineLabel string

	// Out receives the human-facing instructions.
	Out io.Writer

	// OpenBrowser is called with the verification URL. Returning an error is
	// not fatal — the URL has already been printed, and on a headless machine
	// failing to open a browser is the expected case rather than a problem.
	OpenBrowser func(url string) error

	// HTTPClient overrides the client used to reach the server.
	HTTPClient *http.Client
}

type devicePair struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceToken struct {
	AccessToken string `json:"access_token"`
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	MachineID   string `json:"machine_id"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Login runs the device flow and saves the resulting enrollment.
//
// It returns the enrollment it wrote, so a caller can report who was signed in
// without reading the file back.
func Login(ctx context.Context, opts LoginOptions) (Enrollment, error) {
	if strings.TrimSpace(opts.Server) == "" {
		return Enrollment{}, errors.New("no server to sign in to")
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	server := strings.TrimRight(opts.Server, "/")

	// Reuse this installation's machine id. Minting a new one on every sign-in
	// would make one laptop look like a fleet, and the deduplication that keys
	// on it would have nothing stable to work with.
	existing, err := Load(opts.DataDir)
	if err != nil {
		return Enrollment{}, err
	}

	pair, err := requestCode(ctx, client, server, existing.MachineID, opts.MachineLabel)
	if err != nil {
		return Enrollment{}, err
	}

	url := pair.VerificationURIComplete
	if url == "" {
		url = pair.VerificationURI
	}
	fmt.Fprintf(opts.Out, "\nOpen %s\nand enter the code  %s\n\n", pair.VerificationURI, pair.UserCode)
	if opts.OpenBrowser != nil {
		// Best effort. The URL is already on screen, so a headless machine
		// loses nothing by this failing.
		_ = opts.OpenBrowser(url)
	}
	fmt.Fprintln(opts.Out, "Waiting for approval…")

	tok, err := pollForToken(ctx, client, server, pair, opts.Out)
	if err != nil {
		return Enrollment{}, err
	}

	enrolled := Enrollment{
		TenantID:  tok.TenantID,
		UserID:    tok.UserID,
		MachineID: existing.MachineID,
		Token:     tok.AccessToken,
		Server:    server,
	}
	if tok.MachineID != "" && tok.MachineID != existing.MachineID {
		// The server bound the token to a different machine than the one that
		// asked. Refusing is right: shipping under an identity the server did
		// not issue for this install is exactly the mismatch that makes an
		// archive untrustworthy.
		return Enrollment{}, fmt.Errorf("server issued a token for machine %s, not %s", tok.MachineID, existing.MachineID)
	}
	if err := Save(opts.DataDir, enrolled); err != nil {
		return Enrollment{}, err
	}
	return enrolled, nil
}

func requestCode(ctx context.Context, client *http.Client, server, machineID, label string) (devicePair, error) {
	body, _ := json.Marshal(map[string]string{"machine_id": machineID, "machine_label": label})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/auth/device/code", bytes.NewReader(body))
	if err != nil {
		return devicePair{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return devicePair{}, fmt.Errorf("reach %s: %w", server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return devicePair{}, fmt.Errorf("server refused to start sign-in: %s", resp.Status)
	}

	var pair devicePair
	if err := json.NewDecoder(resp.Body).Decode(&pair); err != nil {
		return devicePair{}, fmt.Errorf("decode sign-in response: %w", err)
	}
	if pair.DeviceCode == "" || pair.UserCode == "" {
		return devicePair{}, errors.New("server returned an incomplete sign-in response")
	}
	if pair.Interval <= 0 {
		pair.Interval = 5
	}
	return pair, nil
}

func pollForToken(ctx context.Context, client *http.Client, server string, pair devicePair, out io.Writer) (deviceToken, error) {
	interval := time.Duration(pair.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(max(pair.ExpiresIn, 60)) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return deviceToken{}, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return deviceToken{}, errors.New("sign-in timed out; run login again")
		}

		tok, err := pollOnce(ctx, client, server, pair.DeviceCode)
		if err != nil {
			return deviceToken{}, err
		}
		switch tok.Error {
		case "":
			return tok, nil
		case "authorization_pending":
			// Keep waiting. This is the normal state, not a problem.
		case "slow_down":
			// The server sets the pace; obeying it is the contract.
			interval += 5 * time.Second
		case "access_denied":
			return deviceToken{}, errors.New("sign-in was denied")
		case "expired_token":
			return deviceToken{}, errors.New("that code expired; run login again")
		default:
			return deviceToken{}, fmt.Errorf("sign-in failed: %s", firstNonEmpty(tok.ErrorDescription, tok.Error))
		}
	}
}

func pollOnce(ctx context.Context, client *http.Client, server, deviceCode string) (deviceToken, error) {
	body, _ := json.Marshal(map[string]string{"device_code": deviceCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/auth/device/token", bytes.NewReader(body))
	if err != nil {
		return deviceToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return deviceToken{}, fmt.Errorf("reach %s: %w", server, err)
	}
	defer resp.Body.Close()

	var tok deviceToken
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return deviceToken{}, fmt.Errorf("decode poll response: %w", err)
	}
	return tok, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "unknown error"
}

// OpenInBrowser is the default OpenBrowser for LoginOptions. It is best effort
// by design; see the field's documentation.
func OpenInBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		// rundll32 rather than `cmd /c start`, which treats the first quoted
		// argument as a window title and mangles a URL containing an ampersand.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// Logout forgets the credential and returns this installation to local capture.
//
// The machine id survives, and so does every turn already in the archive. This
// signs out; it does not delete history, and conflating those would make
// signing out a destructive act.
func Logout(dataDir string) (Enrollment, error) {
	e, err := Load(dataDir)
	if err != nil {
		return Enrollment{}, err
	}
	e.TenantID, e.UserID = LocalTenant, LocalUser
	e.Token, e.Server = "", ""
	return e, Save(dataDir, e)
}
