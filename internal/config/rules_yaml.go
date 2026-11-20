package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var ruleSelectors = map[string]bool{
	"source_type": true, "source_name": true, "sender_contains": true,
	"contains": true, "regex": true, "github_reason": true,
}

// Decode the effective rules so YAML aliases, merges, and local overrides have
// the same precedence here as in the final Config decode.
func validateRuleNodes(root *yaml.Node, beforeExpansion bool) error {
	var effective struct {
		Rules []map[string]yaml.Node `yaml:"rules"`
	}
	if err := root.Decode(&effective); err != nil {
		return err
	}
	for i, rule := range effective.Rules {
		for key, node := range rule {
			if !ruleSelectors[key] {
				if !beforeExpansion && key != "name" && key != "action" && key != "priority" {
					return fmt.Errorf("rules[%d]: unknown routing rule field %q", i, key)
				}
				continue
			}
			var selector string
			if err := node.Decode(&selector); err != nil {
				return fmt.Errorf("rules[%d].%s: %w", i, key, err)
			}
			if strings.TrimSpace(selector) == "" {
				return fmt.Errorf("rules[%d].%s: supplied selector must be nonempty", i, key)
			}
			if beforeExpansion {
				for _, match := range envPattern.FindAllStringSubmatch(selector, -1) {
					if strings.TrimSpace(os.Getenv(match[1])) == "" {
						return fmt.Errorf("rules[%d].%s requires nonempty environment variable %s", i, key, match[1])
					}
				}
			}
		}
	}
	return nil
}
