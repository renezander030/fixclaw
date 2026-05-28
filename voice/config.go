//go:build voice

package voice

import "time"

type Config struct {
	Enabled         bool          `yaml:"enabled"`
	ListenAddr      string        `yaml:"listen_addr"`
	PublicBaseURL   string        `yaml:"public_base_url"`
	Auth            AuthConfig    `yaml:"auth"`
	Dograh          DograhConfig  `yaml:"dograh"`
	PreCall         PreCallConfig `yaml:"pre_call"`
	RecordingsDir   string        `yaml:"recordings_dir"`
	DefaultWorkflow string        `yaml:"default_workflow"`
}

type AuthConfig struct {
	Method   string `yaml:"method"`
	TokenEnv string `yaml:"token_env"`
}

type DograhConfig struct {
	BaseURL    string `yaml:"base_url"`    // prod
	StagingURL string `yaml:"staging_url"` // staging
	APIKeyEnv  string `yaml:"api_key_env"`
}

type PreCallConfig struct {
	Enabled      bool          `yaml:"enabled"`
	TimeoutMS    int           `yaml:"timeout_ms"`
	Lookups      []LookupSpec  `yaml:"lookups"`
	RoutingRules []RoutingRule `yaml:"routing_rules"`
}

type LookupSpec struct {
	Source string `yaml:"source"`
	Action string `yaml:"action"`
	URL    string `yaml:"url"`
	Header string `yaml:"header"`
}

type RoutingRule struct {
	If       string `yaml:"if"`
	Workflow string `yaml:"workflow"`
	Default  string `yaml:"default"`
}

func (c Config) Timeout() time.Duration {
	if c.PreCall.TimeoutMS == 0 {
		return 300 * time.Millisecond
	}
	return time.Duration(c.PreCall.TimeoutMS) * time.Millisecond
}
