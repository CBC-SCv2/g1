package rules

import (
	"github.com/rs/zerolog/log"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

func Facebook() *config.Rule {
	// define rule
	r := config.Rule{
		Description: "facebook",
		RuleID:      "facebook",
		Regex: generateSemiGenericRegex([]string{"facebook"},
			hex32),
		SecretGroup: 1,
		Keywords:    []string{"facebook"},
	}

	// validate
	tps := []string{"facebookToken := \"" + sampleHex32Token + "\""}
	d := detect.NewDetector(config.Config{
		Rules: []*config.Rule{&r},
	})
	for _, tp := range tps {
		if len(d.DetectString(tp)) != 1 {
			log.Fatal().Msg("Failed to validate facebook")
		}
	}

	return &r
}