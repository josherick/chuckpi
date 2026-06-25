package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	defaultBaseURL = "https://llm.sherick.me"
	wakeTimeout     = 5 * time.Minute
	pollInterval    = 10 * time.Second
)

var (
	baseURL = defaultBaseURL
	modelID string
)

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.StringVar(&baseURL, "base-url", defaultBaseURL, "base URL for the LLM API")
	fs.StringVar(&modelID, "model", "", "model ID to use (skips model selection)")
	fs.SetOutput(io.Discard)
	fs.Parse(os.Args[1:])

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

	// Use model.ID for display (comparable), store full modelDetails
	selected := modelID
	if selected == "" {
		if len(models) == 1 {
			selected = models[0].ID
		} else {
			opts := make([]huh.Option[string], len(models))
			for i, m := range models {
				opts[i] = huh.NewOption(m.ID, m.ID)
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
	}

	// Get the full model details for the selected ID
	selectedModel := modelsByID(selected)
	if err := configurePi(selectedModel, key); err != nil {
		fmt.Fprintln(os.Stderr, "failed to configure pi:", err)
		os.Exit(1)
	}

	pi, err := exec.LookPath("pi")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pi not found in PATH")
		os.Exit(1)
	}
	// Filter out --model and its value from args passed to pi
	// (configurePi already sets it as the default)
	args := []string{"pi"}
	for i, arg := range fs.Args() {
		if arg == "--model" || arg == "-model" {
			// skip this arg and the next one (the model value)
			if i+1 < len(fs.Args()) {
				i++
			}
			continue
		}
		args = append(args, arg)
	}
	if err := syscall.Exec(pi, args, os.Environ()); err != nil {
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

// modelDetails holds the model info with dynamic context window
type modelDetails struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Input         []string  `json:"input"`
	ContextWindow int       `json:"contextWindow"`
	Tags          []string  `json:"tags,omitempty"`
}

func parseFullModels(data []fullModel) []modelDetails {
	models := make([]modelDetails, len(data))
	for i, m := range data {
		// Extract context window from meta.n_ctx if available
		ctx := m.Meta.NCTX
		if ctx == 0 {
			ctx = 131072 // default fallback
		}
		// Get input modalities from architecture, fallback to "text"
		input := m.Architecture.InputModalities
		if len(input) == 0 {
			input = []string{"text"}
		}
		
		models[i] = modelDetails{
			ID:            m.ID,
			Name:          m.ID, // use ID as name, or could derive from tags
			Input:         input,
			ContextWindow: ctx,
			Tags:          m.Tags,
		}
	}
	return models
}

// listModels returns full model details from the API
func listModels(key string) ([]modelDetails, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", baseURL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var result struct {
		Data []fullModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	
	models := parseFullModels(result.Data)
	// Cache models for lookup
	for _, m := range models {
		allModels[m.ID] = m
	}
	return models, nil
}

// modelsByID finds a model by ID (used for TUI selection)
func modelsByID(id string) modelDetails {
	// Use a map for lookup
	return allModels[id]
}

// fullModel holds the complete model metadata from the API
type fullModel struct {
	ID       string    `json:"id"`
	Tags     []string  `json:"tags"`
	Aliases  []string  `json:"aliases"`
	Architecture struct {
		InputModalities   []string `json:"input_modalities"`
		OutputModalities  []string `json:"output_modalities"`
	} `json:"architecture"`
	Meta struct {
		NCTX    int `json:"n_ctx"`
		NParam  int `json:"n_params"`
		Size    int `json:"size"`
		VocabType int `json:"vocab_type"`
		NVocab   int `json:"n_vocab"`
	} `json:"meta,omitempty"`
	Status struct {
		Value string `json:"value"`
		Args  []string `json:"args"`
	} `json:"status"`
}

// allModels caches the models from listModels for lookup
var allModels map[string]modelDetails

func init() {
	allModels = make(map[string]modelDetails)
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

func configurePi(modelDetails modelDetails, key string) error {
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
	
	// Build model name with optional tags
	modelName := "chuck / " + modelDetails.ID
	if len(modelDetails.Tags) > 0 {
		tags := strings.Join(modelDetails.Tags, ", ")
		modelName = modelName + " [" + tags + "]"
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
			ID:            modelDetails.ID,
			Name:          modelName,
			Reasoning:     false,
			Input:         modelDetails.Input,
			ContextWindow: modelDetails.ContextWindow,
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
	s.DefaultModel = modelDetails.ID
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
