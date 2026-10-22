package server

// LoadConfig parses and validates YAML configuration from disk.
func LoadConfig(path string) (*Config, error) { return LoadConfigProfile(path, "") }
