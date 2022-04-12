package rules

import (
	"github.com/rs/zerolog/log"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

func HubSpot() *config.Rule {
	// define rule
	r := config.Rule{
		Description: "HubSpot API Token",
		RuleID:      "hubspot-api-key",
		Regex: generateSemiGenericRegex([]string{"hubspot"},
			`[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}`),
		SecretGroup: 1,
		Keywords:    []string{"hubspot"},
	}

	// validate
	tps := []string{
		`const hubspotKey = "12345678-ABCD-ABCD-ABCD-1234567890AB"`,
	}
	config := config.Config{}
	config.Rules = append(config.Rules, &r)
	d := detect.NewDetector(config)
	for _, tp := range tps {
		if len(d.DetectString(tp)) != 1 {
			log.Fatal().Msg("Failed to validate hubspot-api-key")
		}
	}

	return &r
}
