package router

import (
	"fmt"

	"cashapp-poc/internal/models"
)

const (
	AutoPost             = "auto_post"
	HumanReview          = "human_review"
	ManualInvestigation  = "manual_investigation"
)

func Route(confidence float64, guardrails *models.GuardrailCheck, unmatchedAmount float64) string {
	if guardrails != nil && !guardrails.Passed {
		fmt.Println("  [ROUTER] → MANUAL INVESTIGATION (guardrails failed)")
		return ManualInvestigation
	}

	if confidence >= 0.95 && unmatchedAmount == 0 {
		fmt.Println("  [ROUTER] → AUTO-POST (confidence ≥ 0.95, zero unmatched)")
		return AutoPost
	}

	if confidence >= 0.70 {
		fmt.Printf("  [ROUTER] → HUMAN REVIEW (confidence %.2f, suggestion pre-filled)\n", confidence)
		return HumanReview
	}

	fmt.Printf("  [ROUTER] → MANUAL INVESTIGATION (confidence %.2f too low)\n", confidence)
	return ManualInvestigation
}
