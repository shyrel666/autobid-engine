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

// 编造的投标人事实换个说法（借招标复述开头、省略"我方"、写案例或人员资格）不得被豁免。
func TestVerifySectionClaims_RephrasedBidderFactsNotExempt(t *testing.T) {
	cases := []struct{ name, sent string }{
		{"restatement_prefix_completed_fact", "依据招标文件要求，投标人已完成同类项目12个。"},
		{"restatement_prefix_track_record", "针对采购需求，项目团队曾为省级政务云平台完成等保三级测评。"},
		{"restatement_prefix_cert_no", "按照招标文件要求，投标人具备二级资质（证书编号：AF-2024-0001）。"},
		{"deployment_case_without_self_cue", "该方案已在杭州、宁波两地政务平台稳定运行三年。"},
		{"named_personnel_qualification", "拟派项目经理张三具有十年同类系统集成经验。"},
		{"cumulative_track_record", "公司累计承建智慧园区项目三十余个。"},
		{"award", "本平台荣获省级科技进步二等奖。"},
		{"we_self_cue", "我们自主研发了统一视频汇聚平台。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := VerifySectionClaims(VerifyClaimsInput{SectionID: 1, ContentMD: tc.sent})
			if len(res.Claims) != 1 {
				t.Fatalf("expected 1 claim, got %+v", res.Claims)
			}
			c := res.Claims[0]
			if c.ClaimType != ClaimTypeBidderFact || c.Status != ClaimStatusUngrounded {
				t.Fatalf("got %s/%s, want %s/%s", c.ClaimType, c.Status, ClaimTypeBidderFact, ClaimStatusUngrounded)
			}
			if res.Verdict != WritingVerdictFailed || res.Metrics.UngroundedClaimCount != 1 {
				t.Fatalf("expected failed verdict with 1 ungrounded claim, got %s %+v", res.Verdict, res.Metrics)
			}
		})
	}
}

// 真正的招标复述、情态要求句与技术叙述仍然豁免，避免误拦。
func TestVerifySectionClaims_RestatementsAndNarrativeStayExempt(t *testing.T) {
	sents := []string{
		"响应要求 REQ-PM「项目经理要求」（招标文件要求：投标人须为本项目配备项目经理一名，须具备信息系统项目管理师资格，并提供近 6 个月社保缴纳记录）。",
		"响应要求 REQ-PERF「类似项目业绩」（招标文件要求：投标人须提供近三年 2 个类似安防项目业绩，须提供合同关键页及验收报告）。",
		"招标文件要求投标人须已取得安全技术防范系统设计施工二级资质。",
		"招标文件（招标编号：ZB-2026-001）要求投标人具备二级及以上资质。",
		"我方承诺完全满足上述要求。",
		"项目经理须具备五年以上系统集成管理经验。",
		"智慧园区安防系统项目经理驻场统筹深化设计、施工质量控制、安全文明施工与竣工归档。",
		"本方案采用感知接入、边缘分析、安防平台与统一运维四层解耦架构。",
	}
	for _, s := range sents {
		res := VerifySectionClaims(VerifyClaimsInput{SectionID: 1, ContentMD: s})
		if len(res.Claims) != 1 || res.Claims[0].Status != ClaimStatusExempt {
			t.Errorf("expected exempt claim for %q, got %+v", s, res.Claims)
		}
	}
}

// 业绩案例按严格客观事实处理：绑定可采企业证据可通过，只绑定知识库证据不行。
func TestVerifySectionClaims_TrackRecordNeedsAdmissibleEvidence(t *testing.T) {
	facts := []GroundedFactSource{{
		AdmissibilityItemID: 102,
		EvidenceDomain:      "performance",
		EvidenceID:          21,
		Code:                "PERF-PARK-2024",
		Title:               "华东智慧园区数字孪生与态势感知平台建设项目",
		ProjectName:         "华东智慧园区数字孪生与态势感知平台建设项目",
		SubjectType:         "bidder",
		ClientName:          "华东高新区管委会",
		Verdict:             Admissible,
	}}
	kbs := []GroundedKBSource{{EvidenceID: 9, Status: "verified", Relation: "supports", Excerpt: "华东高新区态势感知平台运行报告"}}

	res := VerifySectionClaims(VerifyClaimsInput{
		SectionID:       1,
		ContentMD:       "项目团队曾承建华东智慧园区数字孪生与态势感知平台建设项目[EVID:102]。",
		AdmissibleFacts: facts,
	})
	if len(res.Claims) != 1 || res.Claims[0].Status != ClaimStatusGrounded {
		t.Fatalf("expected grounded track record with admissible evidence, got %+v", res.Claims)
	}

	res = VerifySectionClaims(VerifyClaimsInput{
		SectionID:  1,
		ContentMD:  "该平台已在华东高新区稳定运行三年[KB:9]。",
		VerifiedKB: kbs,
	})
	if len(res.Claims) != 1 || res.Claims[0].Status != ClaimStatusUngrounded ||
		!strings.Contains(strings.Join(res.Claims[0].Reasons, ";"), "strict_fact_requires_admissible_evidence") {
		t.Fatalf("expected KB-only track record to be rejected, got %+v", res.Claims)
	}
}
