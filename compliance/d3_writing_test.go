package compliance

import (
	"strings"
	"testing"
)

func floatPtr(v float64) *float64 { return &v }

func TestRenderGroundedDraftAndVerifySectionClaims_GoldenPass(t *testing.T) {
	facts := []GroundedFactSource{
		{
			AdmissibilityItemID:  101,
			RequirementCode:      "REQ-QUAL-01",
			EvidenceDomain:       "credential",
			EvidenceID:           11,
			Code:                 "CRED-ISO27001",
			Title:                "ISO/IEC 27001信息安全管理体系认证证书",
			SubjectName:          "星河智联科技有限公司",
			SubjectType:          "bidder",
			CertificateNo:        "ISO27001-2025-8899",
			QualificationLevel:   "一级",
			Scope:                "智慧园区安全态势感知与零信任接入平台研发与运维",
			Verdict:              Admissible,
			PageLabels:           []string{"附件3-P12"},
		},
		{
			AdmissibilityItemID:  102,
			RequirementCode:      "REQ-PERF-01",
			EvidenceDomain:       "performance",
			EvidenceID:           21,
			Code:                 "PERF-PARK-2024",
			Title:                "华东智慧园区数字孪生与态势感知平台建设项目",
			ProjectName:          "华东智慧园区数字孪生与态势感知平台建设项目",
			SubjectName:          "星河智联科技有限公司",
			SubjectType:          "bidder",
			ClientName:           "华东高新区管委会",
			ContractAmount:       1280.00,
			AcceptanceDate:       "2024-11-15",
			Scope:                "数字孪生引擎、零信任网关、物联边缘节点接入",
			Verdict:              Admissible,
			PageLabels:           []string{"附件4-P01~P08"},
		},
	}
	commits := []GroundedCommitmentSource{
		{
			ID:             301,
			CommitmentCode: MetricDuration,
			Category:       MetricDuration,
			Label:          "总工期",
			Dimension:      DimDuration,
			CommittedValue: floatPtr(90),
			CommittedUnit:  "日历天",
			CommittedRaw:   "90日历天",
			Status:         "confirmed",
		},
		{
			ID:             302,
			CommitmentCode: MetricSLAResponse,
			Category:       MetricSLAResponse,
			Label:          "故障响应时限",
			Dimension:      DimTime,
			CommittedValue: floatPtr(15),
			CommittedUnit:  "分钟",
			CommittedRaw:   "15分钟",
			Status:         "signed_off",
		},
	}

	templateMD := strings.Join([]string{
		"## 1.1 智慧园区态势感知与零信任总体方案",
		"招标文件要求构建智慧园区安全态势感知平台、数字孪生引擎与零信任网关，实现物联边缘节点高并发采集。",
		"针对采购人智慧园区数字孪生引擎与零信任接入需求，本方案设计分层解耦的流式采集与微隔离策略架构，保障边缘节点低时延汇聚。",
		"- [[EVID:101]]",
		"- [[EVID:PERF-PARK-2024]]",
		"- [[COMMIT:total_duration]]",
		"- [[COMMIT:302]]",
	}, "\n\n")

	rendered := RenderGroundedDraft(RenderGroundedInput{
		TemplateMD:                  templateMD,
		AdmissibleFacts:             facts,
		Commitments:                 commits,
		AllowedAdmissibilityItemIDs: []int64{101, 102},
		AllowedCommitmentIDs:        []int64{301, 302},
	})
	if len(rendered.Unresolved) != 0 {
		t.Fatalf("expected 0 unresolved tokens, got %+v", rendered.Unresolved)
	}
	if !strings.Contains(rendered.RenderedMD, "[EVID:101]") || !strings.Contains(rendered.RenderedMD, "[EVID:102]") {
		t.Fatalf("expected [EVID:101] and [EVID:102] in rendered markdown, got:\n%s", rendered.RenderedMD)
	}
	if !strings.Contains(rendered.RenderedMD, "[COMMIT:301]") || !strings.Contains(rendered.RenderedMD, "[COMMIT:302]") {
		t.Fatalf("expected [COMMIT:301] and [COMMIT:302] in rendered markdown, got:\n%s", rendered.RenderedMD)
	}

	res := VerifySectionClaims(VerifyClaimsInput{
		SectionID:                   1,
		SectionCode:                 "1.1",
		SectionTitle:                "智慧园区态势感知与零信任总体方案",
		ContentMD:                   rendered.RenderedMD,
		Requirements: []Requirement{
			{
				ID:          1,
				Code:        "REQ-TECH-01",
				Category:    "scoring_item",
				Title:       "智慧园区数字孪生引擎与零信任安全态势感知",
				Description: "支持物联边缘节点流式采集与微隔离策略，总工期不超过90日历天，故障响应时间不超过15分钟。",
			},
		},
		AdmissibleFacts:             facts,
		Commitments:                 commits,
		AllowedAdmissibilityItemIDs: []int64{101, 102},
		AllowedCommitmentIDs:        []int64{301, 302},
	})

	if res.Verdict != WritingVerdictPassed {
		t.Fatalf("expected passed verdict, got %s, reasons=%+v", res.Verdict, res.Reasons)
	}
	if res.Metrics.UngroundedClaimCount != 0 {
		t.Fatalf("expected 0 ungrounded claims, got %d (%+v)", res.Metrics.UngroundedClaimCount, res.Reasons)
	}
	if res.Metrics.FactBindingRate != 1.0 {
		t.Fatalf("expected FactBindingRate=1.0, got %v", res.Metrics.FactBindingRate)
	}
	if res.Metrics.CommitmentBindingRate != 1.0 {
		t.Fatalf("expected CommitmentBindingRate=1.0, got %v", res.Metrics.CommitmentBindingRate)
	}
	if res.Metrics.SpecificityRatio < 0.5 {
		t.Fatalf("expected SpecificityRatio >= 0.5, got %v", res.Metrics.SpecificityRatio)
	}
}

func TestVerifySectionClaims_DefectsRejected(t *testing.T) {
	facts := []GroundedFactSource{
		{
			AdmissibilityItemID: 101,
			EvidenceDomain:      "credential",
			EvidenceID:          11,
			Code:                "CRED-ISO9001",
			Title:               "质量管理体系认证证书ISO9001",
			SubjectName:         "星河智联科技有限公司",
			SubjectType:         "bidder",
			CertificateNo:       "ISO9001-2024-001",
			Scope:               "信息系统集成服务",
			Verdict:             Admissible,
		},
	}
	commits := []GroundedCommitmentSource{
		{
			ID:             301,
			CommitmentCode: MetricDuration,
			Category:       MetricDuration,
			Label:          "总工期",
			Dimension:      DimDuration,
			CommittedValue: floatPtr(90),
			CommittedUnit:  "日历天",
			CommittedRaw:   "90日历天",
			Status:         "confirmed",
		},
	}

	t.Run("ungrounded_bidder_fact_and_commitment", func(t *testing.T) {
		md := strings.Join([]string{
			"我公司持有CMMI5级软件能力成熟度认证证书（证书编号：CMMI5-9999），并承担过某省政务云平台项目，合同金额2600万元。",
			"我方承诺本项目总工期为 60 日历天，免费质保期为 5 年。",
		}, "\n\n")
		res := VerifySectionClaims(VerifyClaimsInput{
			SectionID:       1,
			ContentMD:       md,
			AdmissibleFacts: facts,
			Commitments:     commits,
		})
		if res.Verdict != WritingVerdictFailed {
			t.Fatalf("expected failed verdict")
		}
		if res.Metrics.UngroundedFactSentences != 1 || res.Metrics.UngroundedCommitmentSentences != 1 {
			t.Fatalf("expected 1 ungrounded fact and 1 ungrounded commitment, got %+v", res.Metrics)
		}
	})

	t.Run("mismatched_fact_and_commitment_values", func(t *testing.T) {
		// 绑定了 ISO9001 的 EVID:101 却伪造证书号 CMMI5-9999；绑定了 90日历天 的 COMMIT:301 却写 60日历天
		md := strings.Join([]string{
			"我公司持有质量管理体系认证证书ISO9001（证书编号：CMMI5-9999）[EVID:101]。",
			"我方承诺总工期为 60 日历天 [COMMIT:301]。",
		}, "\n\n")
		res := VerifySectionClaims(VerifyClaimsInput{
			SectionID:       1,
			ContentMD:       md,
			AdmissibleFacts: facts,
			Commitments:     commits,
		})
		if res.Verdict != WritingVerdictFailed {
			t.Fatalf("expected failed verdict")
		}
		if res.Metrics.UngroundedClaimCount != 2 {
			t.Fatalf("expected 2 ungrounded claims, got %d (%+v)", res.Metrics.UngroundedClaimCount, res.Reasons)
		}
	})

	t.Run("boilerplate_and_low_specificity_rejected", func(t *testing.T) {
		md := defaultBoilerplateTemplates[0]
		res := VerifySectionClaims(VerifyClaimsInput{
			SectionID: 1,
			ContentMD: md,
			Requirements: []Requirement{
				{
					ID:          1,
					Code:        "REQ-01",
					Title:       "数字孪生底座与边缘网关热备切换",
					Description: "支持边缘网关双机热备切换、物联协议解析与三维点云渲染。",
				},
			},
		})
		if res.Verdict != WritingVerdictFailed {
			t.Fatalf("expected failed verdict on boilerplate + low specificity")
		}
		if res.Metrics.BoilerplateSimilarityMax <= DefaultMaxBoilerplateSimilarity {
			t.Fatalf("expected high boilerplate similarity, got %v", res.Metrics.BoilerplateSimilarityMax)
		}
		if res.Metrics.SpecificityRatio >= DefaultMinSpecificityRatio {
			t.Fatalf("expected low specificity ratio, got %v", res.Metrics.SpecificityRatio)
		}
	})
}
