package envload

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Load searches for a .env file (checking cwd, examples/.env, and parent directories)
// and sets any defined environment variables that are not already set.
// Relative paths for GOOGLE_APPLICATION_CREDENTIALS are automatically resolved to
// absolute paths relative to the directory containing the .env file.
func Load() error {
	candidates := []string{
		".env",
		"examples/.env",
		"../examples/.env",
		"../../examples/.env",
	}

	// Also check if an explicit path is in ENV_FILE
	if custom := os.Getenv("ENV_FILE"); custom != "" {
		candidates = append([]string{custom}, candidates...)
	}

	var foundPath string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			foundPath = p
			break
		}
	}

	if foundPath == "" {
		return nil // No .env file found; proceed with environment
	}

	absEnvDir, err := filepath.Abs(filepath.Dir(foundPath))
	if err != nil {
		absEnvDir = filepath.Dir(foundPath)
	}

	file, err := os.Open(foundPath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if _, set := os.LookupEnv(key); set {
			continue // the environment wins over .env
		}

		// Strip surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}

		// Resolve relative credential path
		if key == "GOOGLE_APPLICATION_CREDENTIALS" && val != "" && !filepath.IsAbs(val) {
			val = filepath.Join(absEnvDir, val)
		}

		// Set environment variable
		_ = os.Setenv(key, val)
	}

	return scanner.Err()
}
