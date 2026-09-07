package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/assylkhan/tg-vacancy-filter/internal/config"
	tgclient "github.com/assylkhan/tg-vacancy-filter/internal/telegram"
)

const modelsEndpoint = "https://generativelanguage.googleapis.com/v1beta/models"

// Doctor runs every preflight check and prints a report. It answers the two
// questions that actually break deploys: which account am I signed in as, and
// can it reach the channels and the destination.
//
// testSend additionally posts a message to DESTINATION, which is the only way
// to prove write access.
func Doctor(ctx context.Context, log *slog.Logger, testSend bool) error {
	// Keep the report readable: the checks print to stdout, the machinery
	// keeps logging to stderr.
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	e, err := setup(ctx, quiet, nil)
	if err != nil {
		fmt.Println("✗ config:", err)
		return err
	}
	defer e.close()

	fmt.Println("=== config ===")
	fmt.Printf("  profile        %s\n", e.cfg.ProfilePath)
	fmt.Printf("  threshold      %d/100\n", e.cfg.MatchThreshold)
	fmt.Printf("  channels       %d configured\n", len(e.cfg.SourceChannels))
	fmt.Printf("  destination    %s\n", e.cfg.Destination)
	fmt.Printf("  poll state     %s\n", e.cfg.PollStatePath)
	if e.cfg.PollBootstrapSince.IsZero() {
		fmt.Printf("  bootstrap      not set (new channels start from now)\n")
	} else {
		fmt.Printf("  bootstrap      %s\n", e.cfg.PollBootstrapSince.Format("2006-01-02"))
	}
	fmt.Printf("  max runtime    %s\n", e.cfg.PollMaxRuntime)

	fmt.Println("\n=== gemini ===")
	checkGemini(ctx, e.cfg)

	fmt.Println("\n=== telegram ===")
	fmt.Printf("  session from   %s\n", e.sessionSource)

	return e.client.Run(ctx, func(ctx context.Context) error {
		s, err := e.connect(ctx)
		if err != nil {
			fmt.Println("  ✗ connect:", err)
			return err
		}
		fmt.Printf("  ✓ signed in as id=%d username=@%s name=%q\n",
			s.self.ID, s.self.Username, s.self.FirstName)
		fmt.Printf("  ✓ destination %q resolved\n", e.cfg.Destination)

		fmt.Println("\n=== source channels ===")
		checkChannels(ctx, s.api, e.cfg)

		if testSend {
			fmt.Println("\n=== test message ===")
			text := fmt.Sprintf("🩺 tg-vacancy-filter: проверка связи (%s)",
				time.Now().Format("2006-01-02 15:04:05"))
			if err := s.send.Send(ctx, text); err != nil {
				fmt.Println("  ✗ send failed:", err)
				return err
			}
			fmt.Println("  ✓ sent — проверь чат назначения")
		}
		return nil
	})
}

// checkGemini validates the API key and reports whether the configured models
// are actually served. Model availability on the free tier changes often
// enough that guessing a name is a real failure mode.
func checkGemini(ctx context.Context, cfg *config.Config) {
	models, err := listModels(ctx, cfg.GeminiAPIKey)
	if err != nil {
		fmt.Println("  ✗", err)
		return
	}
	fmt.Printf("  ✓ api key valid — %d models support generateContent\n", len(models))

	available := make(map[string]bool, len(models))
	for _, m := range models {
		available[m] = true
	}
	reportModel("primary ", cfg.GeminiModel, available)
	for _, name := range strings.Split(cfg.GeminiModelFallback, ",") {
		if name = strings.TrimSpace(name); name != "" {
			reportModel("fallback", name, available)
		}
	}

	fmt.Println("  candidates:")
	for _, m := range models {
		if strings.HasPrefix(m, "gemma") || strings.Contains(m, "flash") {
			fmt.Printf("    - %s\n", m)
		}
	}
}

func reportModel(label, name string, available map[string]bool) {
	if available[name] {
		fmt.Printf("  ✓ %s %s\n", label, name)
		return
	}
	fmt.Printf("  ✗ %s %s — NOT served; pick one from the list below\n", label, name)
}

func listModels(ctx context.Context, apiKey string) ([]string, error) {
	u := fmt.Sprintf("%s?key=%s&pageSize=200", modelsEndpoint, url.QueryEscape(apiKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: list models: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("gemini: decode models response: %w", err)
	}
	if body.Error.Code != 0 {
		return nil, fmt.Errorf("gemini: %d %s", body.Error.Code, body.Error.Message)
	}

	var out []string
	for _, m := range body.Models {
		for _, method := range m.Methods {
			if method == "generateContent" {
				out = append(out, strings.TrimPrefix(m.Name, "models/"))
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func checkChannels(ctx context.Context, api *tg.Client, cfg *config.Config) {
	found, missing, err := tgclient.ResolveChannelPeers(ctx, api, cfg.SourceChannels)
	if err != nil {
		fmt.Println("  ✗ resolve:", err)
		return
	}

	ids := make([]int64, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		ch := found[id].Channel
		handle := "private"
		if ch.Username != "" {
			handle = "@" + ch.Username
		}
		fmt.Printf("  ✓ %-14d %-40s %s\n", id, truncate(ch.Title, 40), handle)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	for _, id := range missing {
		fmt.Printf("  ✗ %-14d not in this account's dialogs — join the channel first\n", id)
	}
	fmt.Printf("\n  %d/%d reachable\n", len(found), len(cfg.SourceChannels))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
