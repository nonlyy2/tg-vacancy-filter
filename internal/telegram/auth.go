package telegram

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

// errNoTTY is returned instead of blocking on stdin when the process has no
// terminal. Without it an unattended run (cron, CI, container) hangs forever
// on the login prompt with no indication of why.
var errNoTTY = errors.New(
	"telegram session missing and stdin is not a terminal: " +
		"set TG_STRING_SESSION, or run the bot locally once to create session.json")

// terminalAuth reads the login code (and optional 2FA password) from stdin.
// It is used only when no session is available; a restored session never
// reaches these methods.
type terminalAuth struct {
	phone  string
	reader *bufio.Reader
	tty    bool
}

// NewTerminalAuth returns an auth.UserAuthenticator that collects secrets from
// the terminal. The phone number is supplied from config; codes / passwords
// are read interactively.
func NewTerminalAuth(phone string) auth.UserAuthenticator {
	return &terminalAuth{
		phone:  phone,
		reader: bufio.NewReader(os.Stdin),
		tty:    stdinIsTerminal(),
	}
}

func (a *terminalAuth) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (a *terminalAuth) Password(_ context.Context) (string, error) {
	if !a.tty {
		return "", errNoTTY
	}
	fmt.Fprint(os.Stderr, "Enter 2FA password: ")
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read 2fa password: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func (a *terminalAuth) Code(_ context.Context, sentCode *tg.AuthSentCode) (string, error) {
	if !a.tty {
		return "", errNoTTY
	}
	fmt.Fprintf(os.Stderr, "\nTelegram sent the code via %T\n", sentCode.Type)
	fmt.Fprint(os.Stderr, "Enter the code you received (check ALL devices AND SMS): ")
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read code: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// SignUp is intentionally disabled: the userbot must target an existing account.
func (a *terminalAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("sign up is not supported; create the Telegram account first")
}

func (a *terminalAuth) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	// We only reach this path during sign up, which we refuse above.
	return nil
}

// stdinIsTerminal reports whether a human can actually answer a prompt. The
// os.ModeCharDevice check is not enough: /dev/null is a character device too,
// and that is exactly what an unattended runner hands the process.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
