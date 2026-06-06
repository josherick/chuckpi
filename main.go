package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
)

const (
	baseURL      = "https://llm.sherick.me"
	wakeTimeout  = 5 * time.Minute
	pollInterval = 10 * time.Second
)

func main() {
	key := os.Getenv("LLM_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "error: LLM_API_KEY not set")
		os.Exit(1)
	}

	if !checkHealth(key) {
		fmt.Println("chuck is offline — sending wake packet...")
		if err := sendWake(key); err != nil {
			fmt.Fprintf(os.Stderr, "wake request failed: %v\n", err)
		}

		ready := spinUntil("Waiting for chuck to boot...", wakeTimeout, func() bool {
			return checkHealth(key)
		})
		if !ready {
			fmt.Fprintln(os.Stderr, "timed out waiting for chuck — is it plugged in?")
			os.Exit(1)
		}
	}

	models, err := listModels(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to list models:", err)
		os.Exit(1)
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "no models found")
		os.Exit(1)
	}

	var selected string
	if len(models) == 1 {
		selected = models[0]
	} else {
		opts := make([]huh.Option[string], len(models))
		for i, m := range models {
			opts[i] = huh.NewOption(m, m)
		}
		if err := huh.NewSelect[string]().
			Title("Select a model").
			Options(opts...).
			Value(&selected).
			Run(); err != nil {
			fmt.Fprintln(os.Stderr, "selection cancelled")
			os.Exit(1)
		}
	}

	if err := configurePi(selected, key); err != nil {
		fmt.Fprintln(os.Stderr, "failed to configure pi:", err)
		os.Exit(1)
	}

	pi, err := exec.LookPath("pi")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pi not found in PATH")
		os.Exit(1)
	}
	if err := syscall.Exec(pi, []string{"pi"}, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "failed to exec pi:", err)
		os.Exit(1)
	}
}

// spinUntil shows a spinner while polling fn every pollInterval until it
// returns true or the timeout is reached.
func spinUntil(title string, timeout time.Duration, fn func() bool) bool {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if fn() {
				done <- true
				return
			}
			time.Sleep(pollInterval)
		}
		done <- false
	}()
	for i := 0; ; i++ {
		select {
		case result := <-done:
			fmt.Printf("\r%s\n", strings.Repeat(" ", len(title)+4))
			return result
		default:
			fmt.Printf("\r%s %s", frames[i%len(frames)], title)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func checkHealth(key string) bool {
	client := &http.Client{Timeout: 8 * time.Second}
	req, _ := http.NewRequest("GET", baseURL+"/health", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var result struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Status == "ok"
}

func sendWake(key string) error {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("POST", baseURL+"/admin/api/wake", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		fmt.Println("(wake already sent recently — just waiting)")
	}
	return nil
}

func listModels(key string) ([]string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", baseURL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	ids := make([]string, len(result.Data))
	for i, m := range result.Data {
		ids[i] = m.ID
	}
	return ids, nil
}

// pi agent config types

type modelsConfig struct {
	Providers map[string]provider `json:"providers"`
}

type provider struct {
	BaseURL    string   `json:"baseUrl"`
	API        string   `json:"api"`
	APIKey     string   `json:"apiKey"`
	AuthHeader bool     `json:"authHeader"`
	Compat     compat   `json:"compat"`
	Models     []model  `json:"models"`
}

type compat struct {
	SupportsDeveloperRole   bool   `json:"supportsDeveloperRole"`
	SupportsReasoningEffort bool   `json:"supportsReasoningEffort"`
	MaxTokensField          string `json:"maxTokensField"`
}

type model struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	ContextWindow int      `json:"contextWindow"`
	Cost          cost     `json:"cost"`
}

type cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

type settings struct {
	LastChangelogVersion string `json:"lastChangelogVersion,omitempty"`
	DefaultProvider      string `json:"defaultProvider"`
	DefaultModel         string `json:"defaultModel"`
	EditorPaddingX       int    `json:"editorPaddingX,omitempty"`
}

func configurePi(modelID, key string) error {
	home, _ := os.UserHomeDir()
	piDir := filepath.Join(home, ".pi", "agent")

	// Load and update models.json, preserving other providers
	modelsPath := filepath.Join(piDir, "models.json")
	var cfg modelsConfig
	if data, err := os.ReadFile(modelsPath); err == nil {
		json.Unmarshal(data, &cfg)
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]provider)
	}
	cfg.Providers["chuck"] = provider{
		BaseURL:    baseURL + "/v1",
		API:        "openai-completions",
		APIKey:     key,
		AuthHeader: true,
		Compat: compat{
			SupportsDeveloperRole:   false,
			SupportsReasoningEffort: false,
			MaxTokensField:          "max_tokens",
		},
		Models: []model{{
			ID:            modelID,
			Name:          "chuck / " + modelID,
			Reasoning:     false,
			Input:         []string{"text"},
			ContextWindow: 131072,
		}},
	}
	if err := writeJSON(modelsPath, cfg); err != nil {
		return fmt.Errorf("models.json: %w", err)
	}

	// Load and update settings.json
	settingsPath := filepath.Join(piDir, "settings.json")
	var s settings
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &s)
	}
	s.DefaultProvider = "chuck"
	s.DefaultModel = modelID
	if err := writeJSON(settingsPath, s); err != nil {
		return fmt.Errorf("settings.json: %w", err)
	}

	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
