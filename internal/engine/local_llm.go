package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const localLLMEndpointEnv = "COGOS_LLM_ENDPOINT"

type LocalLLMBackend string

const (
	LocalLLMBackendOllama       LocalLLMBackend = "ollama"
	LocalLLMBackendOpenAICompat LocalLLMBackend = "openai-compat"
)

type LocalLLMTarget struct {
	BaseURL string
	Backend LocalLLMBackend
	Models  []string
}

func normalizeLocalLLMEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1") {
		endpoint = strings.TrimSuffix(endpoint, "/v1")
	}
	return strings.TrimRight(endpoint, "/")
}

func resolveLocalLLMEndpoint(cfgEndpoint string) string {
	if endpoint := normalizeLocalLLMEndpoint(cfgEndpoint); endpoint != "" {
		return endpoint
	}
	if endpoint := normalizeLocalLLMEndpoint(os.Getenv(localLLMEndpointEnv)); endpoint != "" {
		return endpoint
	}
	return openaiCompatDefaultEndpoint
}

func detectLocalLLMTarget(ctx context.Context, cfgEndpoint string) (LocalLLMTarget, error) {
	baseURL := resolveLocalLLMEndpoint(cfgEndpoint)
	if models, err := probeOllamaModels(ctx, baseURL); err == nil {
		return LocalLLMTarget{
			BaseURL: baseURL,
			Backend: LocalLLMBackendOllama,
			Models:  models,
		}, nil
	} else if models, err2 := probeOpenAICompatModels(ctx, baseURL); err2 == nil {
		return LocalLLMTarget{
			BaseURL: baseURL,
			Backend: LocalLLMBackendOpenAICompat,
			Models:  models,
		}, nil
	} else {
		return LocalLLMTarget{}, fmt.Errorf("local llm unavailable at %s (ollama probe: %v; openai probe: %v)", baseURL, err, err2)
	}
}

func probeOllamaModels(ctx context.Context, baseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeLocalLLMEndpoint(baseURL)+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(tags.Models))
	for _, model := range tags.Models {
		if model.Name != "" {
			out = append(out, model.Name)
		}
	}
	return out, nil
}

func probeOpenAICompatModels(ctx context.Context, baseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeLocalLLMEndpoint(baseURL)+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var result openaiModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(result.Data))
	for _, model := range result.Data {
		if model.ID != "" {
			out = append(out, model.ID)
		}
	}
	return out, nil
}

func buildLocalProvider(target LocalLLMTarget, model string) Provider {
	cfg := ProviderConfig{
		Endpoint: target.BaseURL,
		Model:    model,
		Timeout:  120,
	}
	switch target.Backend {
	case LocalLLMBackendOllama:
		cfg.ContextWindow = 32768
		return NewOllamaProvider("agent-local", cfg)
	default:
		cfg.MaxTokens = openaiCompatDefaultMaxToks
		return NewOpenAICompatProvider("agent-local", cfg)
	}
}

func resolvePreferredLocalModel(models []string, preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		for _, model := range models {
			if model == preferred || strings.HasPrefix(model, preferred) {
				return model
			}
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

var largeLocalModelPattern = regexp.MustCompile(`(^|[^0-9])([0-9]{2,3})b([^0-9]|$)`)

func looksLikeLargeLocalModel(model string) bool {
	m := largeLocalModelPattern.FindStringSubmatch(strings.ToLower(model))
	if len(m) < 3 {
		return false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return false
	}
	return n >= 26
}

func resolveDispatchLocalModel(models []string, preferred string, requested DispatchModel) (string, DispatchModel, string) {
	switch requested {
	case DispatchModel26B:
		for _, model := range models {
			if looksLikeLargeLocalModel(model) {
				return model, DispatchModel26B, ""
			}
		}
		fallback := resolvePreferredLocalModel(models, preferred)
		if fallback == "" {
			return "", DispatchModelE4B, "26b route unavailable: no local models are loaded"
		}
		return fallback, DispatchModelE4B, "26b route unavailable, degraded to e4b"
	default:
		selected := resolvePreferredLocalModel(models, preferred)
		if selected == "" {
			return "", DispatchModelE4B, "no local models are loaded"
		}
		if preferred != "" && selected != preferred {
			return selected, DispatchModelE4B, fmt.Sprintf("configured local model %q not loaded, using %q", preferred, selected)
		}
		return selected, DispatchModelE4B, ""
	}
}
