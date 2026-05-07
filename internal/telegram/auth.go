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
)

// terminalAuth reads the login code (and optional 2FA password) from stdin.
// It is used only on first boot; subsequent boots reuse the session file.
type terminalAuth struct {
	phone  string
	reader *bufio.Reader
}

// NewTerminalAuth returns an auth.UserAuthenticator that collects secrets from
// the terminal. The phone number is supplied from config; codes / passwords
// are read interactively.
func NewTerminalAuth(phone string) auth.UserAuthenticator {
	return &terminalAuth{
		phone:  phone,
		reader: bufio.NewReader(os.Stdin),
	}
}

func (a *terminalAuth) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (a *terminalAuth) Password(_ context.Context) (string, error) {
	fmt.Fprint(os.Stderr, "Enter 2FA password: ")
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read 2fa password: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func (a *terminalAuth) Code(_ context.Context, sentCode *tg.AuthSentCode) (string, error) {
	fmt.Fprintf(os.Stderr, "\n[DEBUG] Telegram server response: code sent via %T\n", sentCode.Type)
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
