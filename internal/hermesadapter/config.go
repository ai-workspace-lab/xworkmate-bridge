package hermesadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func resolveHermesConfiguredModel() string {
	for _, path := range hermesConfigCandidatePaths() {
		if model := readHermesModelFromConfig(path); model != "" {
			return model
		}
	}
	return ""
}

func hermesConfigCandidatePaths() []string {
	candidates := make([]string, 0, 3)
	if hermesHome := strings.TrimSpace(os.Getenv("HERMES_HOME")); hermesHome != "" {
		candidates = append(candidates, filepath.Join(hermesHome, "config.yaml"))
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		candidates = append(candidates, filepath.Join(home, ".hermes", "config.yaml"))
	}
	if userHome, err := os.UserHomeDir(); err == nil {
		userHome = strings.TrimSpace(userHome)
		if userHome != "" {
			candidates = append(candidates, filepath.Join(userHome, ".hermes", "config.yaml"))
		}
	}
	return dedupeStrings(candidates)
}

func readHermesModelFromConfig(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	if model := resolveHermesModelValue(config["model"]); model != "" {
		return model
	}
	if model := strings.TrimSpace(fmt.Sprint(config["model"])); model != "" && model != "<nil>" {
		return model
	}
	return ""
}

func resolveHermesModelValue(value any) string {
	switch model := value.(type) {
	case string:
		return strings.TrimSpace(model)
	case map[string]any:
		for _, key := range []string{"default", "model"} {
			if candidate := strings.TrimSpace(fmt.Sprint(model[key])); candidate != "" && candidate != "<nil>" {
				return candidate
			}
		}
	}
	return ""
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
