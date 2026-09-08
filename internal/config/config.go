// Package config loads and validates environment configuration.
package config

import (
	"fmt"
	"strconv"
)

// Config holds all configuration knobs loaded from the environment.
type Config struct {
	Repo, Approver                                string
	LedgerBucket                                  string
	LedgerAppliedPrefix, LedgerFailedPrefix       string
	LedgerHeadKey, HeartbeatKey, PlanDigestPrefix string
	Workdir, OPTokenFile                          string
	SecretsDir, PluginDir                         string // defaulted
	RequiredCheck                                 string // "plan"
	ExpiryWarnDays                                int    // defaulted 30
	DriftOnly                                     bool
}

// Load reads and validates configuration from the environment via the provided
// getenv function. It returns a Config and a list of all problems found.
// If any problems are found, the Config is not usable and the problem list is non-empty.
func Load(getenv func(string) string) (Config, []string) {
	var problems []string
	cfg := Config{RequiredCheck: "plan"}

	// Load required variables
	requiredVars := []string{
		"REPO", "APPROVER", "LEDGER_BUCKET",
		"LEDGER_APPLIED_PREFIX", "LEDGER_FAILED_PREFIX", "LEDGER_HEAD_KEY",
		"HEARTBEAT_KEY", "PLAN_DIGEST_PREFIX", "WORKDIR", "OP_TOKEN_FILE",
	}

	values := make(map[string]string)
	for _, name := range requiredVars {
		values[name] = getenv(name)
		if values[name] == "" {
			problems = append(problems, fmt.Sprintf("refusing to start: %s is unset", name))
		}
	}

	// Set required fields only if they were provided
	if values["REPO"] != "" {
		cfg.Repo = values["REPO"]
	}
	if values["APPROVER"] != "" {
		cfg.Approver = values["APPROVER"]
	}
	if values["LEDGER_BUCKET"] != "" {
		cfg.LedgerBucket = values["LEDGER_BUCKET"]
	}
	if values["LEDGER_APPLIED_PREFIX"] != "" {
		cfg.LedgerAppliedPrefix = values["LEDGER_APPLIED_PREFIX"]
	}
	if values["LEDGER_FAILED_PREFIX"] != "" {
		cfg.LedgerFailedPrefix = values["LEDGER_FAILED_PREFIX"]
	}
	if values["LEDGER_HEAD_KEY"] != "" {
		cfg.LedgerHeadKey = values["LEDGER_HEAD_KEY"]
	}
	if values["HEARTBEAT_KEY"] != "" {
		cfg.HeartbeatKey = values["HEARTBEAT_KEY"]
	}
	if values["PLAN_DIGEST_PREFIX"] != "" {
		cfg.PlanDigestPrefix = values["PLAN_DIGEST_PREFIX"]
	}
	if values["WORKDIR"] != "" {
		cfg.Workdir = values["WORKDIR"]
	}
	if values["OP_TOKEN_FILE"] != "" {
		cfg.OPTokenFile = values["OP_TOKEN_FILE"]
	}

	// Load optional variables with defaults
	if secretsDir := getenv("SECRETS_DIR"); secretsDir != "" {
		cfg.SecretsDir = secretsDir
	} else {
		cfg.SecretsDir = "/secrets"
	}

	if pluginDir := getenv("TF_PLUGIN_DIR"); pluginDir != "" {
		cfg.PluginDir = pluginDir
	} else {
		cfg.PluginDir = "/opt/tofu-providers"
	}

	if expiryWarnDays := getenv("EXPIRY_WARN_DAYS"); expiryWarnDays != "" {
		if days, err := strconv.Atoi(expiryWarnDays); err == nil {
			cfg.ExpiryWarnDays = days
		} else {
			problems = append(problems, fmt.Sprintf("refusing to start: EXPIRY_WARN_DAYS is not a valid integer: %v", err))
		}
	} else {
		cfg.ExpiryWarnDays = 30
	}

	// Load DRIFT_CHECK - accepts only 0, 1, or unset
	if driftCheck := getenv("DRIFT_CHECK"); driftCheck != "" {
		switch driftCheck {
		case "0":
			cfg.DriftOnly = false
		case "1":
			cfg.DriftOnly = true
		default:
			problems = append(problems, fmt.Sprintf("refusing to start: DRIFT_CHECK accepts only 0, 1, or unset, not %q", driftCheck))
		}
	}

	return cfg, problems
}
