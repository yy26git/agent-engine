package control

import (
	"strings"

	"github.com/emjin/agent-engine/contract"
)

func contractPlanForTest(version int) contract.ImplementationPlan {
	return contract.ImplementationPlan{
		Schema: contract.EpisodeVersion, EpisodeID: "EP-1", Version: version,
		AcceptedReportDigest: strings.Repeat("a", 64),
		SourceBaseline:       strings.Repeat("b", 64), Grant: "read-write",
		ExecutionProfile: "worker", ReviewProfile: "auditor",
		Steps: []contract.PlanStep{{
			StepID: "S1", Findings: []string{"F1"},
			AllowedChangeKinds: []string{"source"}, AcceptanceCriteria: "build passes",
		}},
	}
}

func authForTest(plan contract.ImplementationPlan) contract.PlanAuthorization {
	d, _ := plan.Digest()
	return contract.PlanAuthorization{
		Schema: contract.EpisodeVersion, EpisodeID: "EP-1", PlanVersion: plan.Version,
		PlanDigest: d, ReportDigest: plan.AcceptedReportDigest,
		SourceBaseline: plan.SourceBaseline, Grant: plan.Grant,
		AcceptedBy: "operator", AcceptedAt: "2026-10-10T00:00:00Z",
	}
}
