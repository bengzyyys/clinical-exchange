// 命令 correction_conflict 演示更正遇到版本冲突后的完整处理过程：两名内部
// 使用者先各自取得同一条诊断的第 1 版信息，一人先更正成功产生第 2 版，另一人
// 仍用手中的旧版本号提交不同正文与非空原因，得到 ErrConflict；被拒者重新查看
// 记录的当前内容，在已确认的第 2 版正文基础上重新组织完整正文，用最新版本号
// 再次更正，成功产生第 3 版。
//
// 示例同时区分两种预期失败：
//   - 版本号已过期（内容、原因都合法）→ ErrConflict；
//   - 更正原因全为空白（哪怕版本号就是当前版本号）→ ErrInvalidArgument。
//
// 两种失败都不产生新版本或更正审计，失败返回的 Version 是零值，不能当成已经
// 保存的新版本继续使用。示例还明确区分记录标识、版本标识与整数版本号：
// CorrectRecord 需要的是当前版本的整数版本号 Version.Number。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/correction_conflict
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 同一条合成诊断的三个正式版本正文，以及一次被拒更正的正文。
const (
	contentV1 = "合成诊断：急性支气管炎 J20"
	contentV2 = "合成诊断：急性支气管炎 J20（已听诊确认）"
	reasonV2  = "听诊复核确认体征"

	// rejectedContent/rejectedReason 是第二名使用者基于旧版本号提交的内容：
	// 它永远不会被保存——库不自动合并任何人的修改。
	rejectedContent = "合成诊断：急性支气管炎 J20（疑为肺炎，建议影像）"
	rejectedReason  = "建议影像排查肺炎"

	// 第 3 版由被拒者重新查看后提交：完整正文里保留第 2 版已经确认的
	// “（已听诊确认）”，再追加本次补充，而不是只写追加片段。
	contentV3 = "合成诊断：急性支气管炎 J20（已听诊确认；补充复诊随访安排）"
	reasonV3  = "冲突后重新查看：保留已听诊确认结论，补充复诊随访安排"

	// blankReasonAttempt 只用于演示“原因全为空白”的失败，本身不会被保存。
	blankReasonAttemptContent = "合成诊断：急性支气管炎 J20（空白原因的尝试不会保存）"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-correction-conflict-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	// 两名不同的内部使用者；他们各自独立查看、独立提交更正。
	doctorA := clinical.InternalActor("doctor-a")
	doctorB := clinical.InternalActor("doctor-b")

	// ---- 准备：一名合成患者、一次就诊、一条已生效的诊断（第 1 版）----
	patient, err := store.RegisterPatient(doctorA, "合成患者壬")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctorA, patient.ID, time.Now())
	must("登记就诊", err)
	draft, err := store.CreateDraft(doctorA, patient.ID, encounter.ID,
		clinical.Diagnosis, contentV1)
	must("创建诊断草稿", err)
	v1, err := store.ActivateRecord(doctorA, draft.ID)
	must("生效诊断（第 1 版）", err)

	fmt.Println("准备完成: 1 名合成患者、1 次就诊、1 条已生效诊断")
	fmt.Printf("  记录标识 Record.ID = %s（记录全程不变，更正不换标识）\n", draft.ID)
	fmt.Printf("  第 1 版: 版本标识 Version.ID = %s，整数版本号 Version.Number = %d\n", v1.ID, v1.Number)
	fmt.Println("标识区分: 记录标识标“哪条记录”；版本标识标“哪一个不可变版本”；" +
		"CorrectRecord 要带的是当前版本的整数版本号 Number，不是前两个标识")

	// ---- 两名内部使用者都先取得第 1 版的信息 ----
	// 各自通过内部查询取回当前版本；此刻两人看到的都是第 1 版。
	heldA := mustHistory(store, doctorA, patient.ID, encounter.ID, draft.ID)
	heldB := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	if heldA.CurrentVersion == nil || heldB.CurrentVersion == nil ||
		heldA.CurrentVersion.Number != 1 || heldB.CurrentVersion.Number != 1 ||
		heldA.CurrentVersion.ID != v1.ID || heldB.CurrentVersion.ID != v1.ID {
		die("两名使用者取得的都应是第 1 版（版本标识 %s），实际 A=%+v B=%+v",
			v1.ID, heldA.CurrentVersion, heldB.CurrentVersion)
	}
	fmt.Printf("\n两人各自查看: doctor-a 与 doctor-b 取得同一当前版本——"+
		"版本号=%d，版本标识=%s\n", heldB.CurrentVersion.Number, heldB.CurrentVersion.ID)

	// ---- 第一次更正（成功）：doctor-a 用当时的当前版本号 1 提交 ----
	v2, err := store.CorrectRecord(doctorA, draft.ID, heldA.CurrentVersion.Number,
		contentV2, reasonV2)
	must("doctor-a 第一次更正诊断", err)
	fmt.Printf("\n【第一次更正·成功】doctor-a 以版本号 %d 提交: 记录成为第 %d 版\n",
		heldA.CurrentVersion.Number, v2.Number)
	fmt.Printf("  新版本标识=%s，上一版本标识=%s（指向第 1 版 %s）=%v\n",
		v2.ID, v2.PrevID, v1.ID, v2.PrevID == v1.ID)
	fmt.Printf("  新正文=%q，更正原因=%q\n", v2.Content, v2.Reason)

	// ---- 第二次提交（被拒）：doctor-b 仍用手中的旧版本号 1 ----
	// 正文与原因都非空、合法，唯一的问题是版本号已不是当前版本号。
	rejected, err := store.CorrectRecord(doctorB, draft.ID, heldB.CurrentVersion.Number,
		rejectedContent, rejectedReason)
	switch {
	case err == nil:
		die("doctor-b 用旧版本号更正应当被拒绝，却返回了新版本 %+v", rejected)
	case errors.Is(err, clinical.ErrConflict):
		// 预期失败：返回的 Version 是零值，绝不是“第 2 版”或某个新第 3 版，
		// 不能把它当作已保存结果继续传递。
		if rejected != (clinical.Version{}) {
			die("冲突时返回值应为零值 Version，实际得到 %+v", rejected)
		}
		fmt.Printf("\n【第二次提交·被拒】doctor-b 仍以取得时的版本号 %d 提交不同正文: %v\n",
			heldB.CurrentVersion.Number, err)
		fmt.Printf("  错误类型=ErrConflict；失败返回值是零值版本（版本号=%d、版本标识=%q），"+
			"不是已保存的新版本，不能继续使用\n", rejected.Number, rejected.ID)
	default:
		die("doctor-b 旧版本号更正应返回 ErrConflict，实际得到 %v", err)
	}

	// ---- 核对被拒后的正式记录：仍是第 2 版，历史仍只有两版，无新增更正审计 ----
	afterConflict := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	cur := afterConflict.CurrentVersion
	if cur.Number != 2 || cur.ID != v2.ID || cur.Content != contentV2 ||
		cur.Reason != reasonV2 || cur.PrevID != v1.ID {
		die("冲突后正式记录应仍为第 2 版且正文/原因保留第一次更正结果，实际当前版本=%+v", cur)
	}
	if len(afterConflict.Versions) != 2 {
		die("冲突后历史应仍只有两版，实际有 %d 版", len(afterConflict.Versions))
	}
	if strings.Contains(cur.Content, "影像") || strings.Contains(cur.Content, "肺炎") {
		die("被拒正文不得进入正式记录，实际当前正文=%q", cur.Content)
	}
	correctedSoFar := mustCorrectionAudits(store, doctorA, patient.ID)
	if len(correctedSoFar) != 1 || correctedSoFar[0].ActorID != doctorA.ID {
		die("冲突后应只有 doctor-a 的 1 条更正审计，实际 %+v", correctedSoFar)
	}
	fmt.Printf("被拒后核对: 正式记录仍为第 %d 版（版本标识=%s），正文与原因保留第一次更正结果=%v；"+
		"历史版本数=%d；更正审计数=%d（被拒提交没有新增审计）\n",
		cur.Number, cur.ID, cur.Content == contentV2 && cur.Reason == reasonV2,
		len(afterConflict.Versions), len(correctedSoFar))

	// ---- 另一种预期失败：更正原因全为空白 → ErrInvalidArgument ----
	// 这次版本号用的就是当前版本号 2，参数问题（原因空白）与并发无关；
	// 它同样不产生新版本或更正审计。
	auditsBeforeBlank := len(mustCorrectionAudits(store, doctorA, patient.ID))
	blankReason := " \t\n " // 去掉空白后为空
	blank, err := store.CorrectRecord(doctorB, draft.ID, cur.Number,
		blankReasonAttemptContent, blankReason)
	switch {
	case err == nil:
		die("原因全为空白的更正应当被拒绝，却返回了新版本 %+v", blank)
	case errors.Is(err, clinical.ErrInvalidArgument):
		if blank != (clinical.Version{}) {
			die("参数非法时返回值应为零值 Version，实际得到 %+v", blank)
		}
		fmt.Printf("\n【原因空白·被拒】以当前版本号 %d 提交、但更正原因为全空白 %q: %v\n",
			cur.Number, blankReason, err)
		fmt.Println("  错误类型=ErrInvalidArgument（参数本身不合法，与版本是否过期无关）；同样不产生新版本或审计")
	default:
		die("原因全白应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	afterBlank := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	auditsAfterBlank := len(mustCorrectionAudits(store, doctorA, patient.ID))
	if afterBlank.CurrentVersion.Number != 2 || afterBlank.CurrentVersion.ID != v2.ID ||
		len(afterBlank.Versions) != 2 || auditsAfterBlank != auditsBeforeBlank {
		die("空白原因失败后记录应仍为第 2 版两版历史且审计不增加，实际版本号=%d 历史=%d 审计=%d→%d",
			afterBlank.CurrentVersion.Number, len(afterBlank.Versions), auditsBeforeBlank, auditsAfterBlank)
	}
	fmt.Printf("  失败后核对: 仍为第 %d 版、历史 %d 版、更正审计 %d 条（未增加）\n",
		afterBlank.CurrentVersion.Number, len(afterBlank.Versions), auditsAfterBlank)
	fmt.Printf("两种失败的区别: ErrConflict=内容与原因都合法、仅版本号过期；" +
		"ErrInvalidArgument=原因（或正文）空白等参数问题，在冲突检查之前就被拒绝\n")

	// ---- 被拒者重新查看记录的当前内容 ----
	// 手中的 heldB 停留在第 1 版，已不能用于提交；以重新查到的当前版本为准。
	refreshed := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	latest := refreshed.CurrentVersion
	fmt.Printf("\n被拒后重新查看: doctor-b 看到记录当前为第 %d 版（版本标识=%s）\n",
		latest.Number, latest.ID)
	fmt.Printf("  当前正文=%q，当前更正原因=%q\n", latest.Content, latest.Reason)
	if latest.Number != 2 || latest.ID != v2.ID || latest.Content != contentV2 {
		die("重新查看应得到第 2 版第一次更正后的内容，实际=%+v", latest)
	}

	// ---- 确认要保留的内容后，用最新版本号再次更正 ----
	// 更正提交的是完整正文：库里不会把 rejectedContent 与别人的修改自动合并，
	// 所以新正文必须显式保留第 2 版已确认的“（已听诊确认）”，再写本次补充。
	v3, err := store.CorrectRecord(doctorB, draft.ID, latest.Number, contentV3, reasonV3)
	must("doctor-b 以最新版本号重新更正", err)
	fmt.Printf("\n【第三次提交·成功】doctor-b 重新确认内容后以当前版本号 %d 提交: 记录成为第 %d 版\n",
		latest.Number, v3.Number)
	fmt.Printf("  新版本标识=%s，上一版本标识=%s（指向第 2 版 %s）=%v\n",
		v3.ID, v3.PrevID, v2.ID, v3.PrevID == v2.ID)
	fmt.Printf("  新正文=%q\n", v3.Content)
	fmt.Printf("  保留第 2 版已确认变化（“已听诊确认”）=%v；体现本次补充（“复诊随访安排”）=%v\n",
		strings.Contains(v3.Content, "已听诊确认"), strings.Contains(v3.Content, "复诊随访安排"))
	fmt.Printf("  更正原因=%q；被拒正文（影像/肺炎）没有被合并进来=%v\n",
		v3.Reason, !strings.Contains(v3.Content, "影像") && !strings.Contains(v3.Content, "肺炎"))

	// ---- 最终核对：三版完整打印，前两版未被覆盖；两次成功更正各有审计 ----
	final := mustHistory(store, doctorA, patient.ID, encounter.ID, draft.ID)
	fmt.Printf("\n最终版本历史（记录标识仍为 %s，全程未变；共 %d 版）:\n",
		draft.ID, len(final.Versions))
	for i, v := range final.Versions {
		printVersion(i, v)
	}
	if len(final.Versions) != 3 {
		die("最终应有三版，实际 %d 版", len(final.Versions))
	}
	got := final.Versions
	if got[0].ID != v1.ID || got[0].Number != 1 || got[0].Content != contentV1 ||
		got[0].PrevID != "" || got[0].Reason != "" {
		die("第 1 版被覆盖或字段异常: %+v", got[0])
	}
	if got[1].ID != v2.ID || got[1].Number != 2 || got[1].Content != contentV2 ||
		got[1].PrevID != v1.ID || got[1].Reason != reasonV2 {
		die("第 2 版被覆盖或字段异常: %+v", got[1])
	}
	if got[2].ID != v3.ID || got[2].Number != 3 || got[2].Content != contentV3 ||
		got[2].PrevID != v2.ID || got[2].Reason != reasonV3 {
		die("第 3 版字段异常: %+v", got[2])
	}
	if final.CurrentVersion.Number != 3 || final.CurrentVersion.ID != v3.ID {
		die("当前版本应为第 3 版，实际=%+v", final.CurrentVersion)
	}
	fmt.Println("核对: 第 3 版接在第 2 版之后；第 1、2 版正文、上一版本标识与原因均原样保留，没有被覆盖")

	correctedAudits := mustCorrectionAudits(store, doctorA, patient.ID)
	if len(correctedAudits) != 2 {
		die("两次成功更正应各有 1 条更正审计（共 2 条），实际 %d 条", len(correctedAudits))
	}
	fmt.Printf("更正审计: 共 %d 条，分别由 %s、%s 发起；两次被拒提交均未留下审计\n",
		len(correctedAudits), correctedAudits[0].ActorID, correctedAudits[1].ActorID)
	if correctedAudits[0].ActorID != doctorA.ID || correctedAudits[1].ActorID != doctorB.ID {
		die("更正审计顺序异常: %+v", correctedAudits)
	}
}

// mustHistory 由内部使用者查询指定就诊下的记录，并按记录标识取回这一条记录的
// 完整视图。查询本身失败、或记录不在结果中，都视为示例准备/核对失败并终止。
func mustHistory(store *clinical.Store, actor clinical.Actor, patientID, encounterID, recordID clinical.ID) clinical.RecordHistory {
	rows, err := store.EncounterRecords(actor, patientID, encounterID)
	must("内部使用者查询就诊记录", err)
	for i := range rows {
		if rows[i].Record.ID == recordID {
			return rows[i]
		}
	}
	die("查询结果中找不到记录 %q", recordID)
	return clinical.RecordHistory{}
}

// mustCorrectionAudits 取回该患者的全部更正审计，按发生顺序旧到新。
func mustCorrectionAudits(store *clinical.Store, actor clinical.Actor, patientID clinical.ID) []clinical.AuditEvent {
	events, err := store.AuditEvents(actor, patientID)
	must("查看审计事件", err)
	var out []clinical.AuditEvent
	for _, e := range events {
		if e.Action == clinical.ActionCorrected {
			out = append(out, e)
		}
	}
	return out
}

// printVersion 逐版打印版本号、版本标识、上一版本标识、更正原因与完整正文。
func printVersion(index int, v clinical.Version) {
	prev := "无（首版由草稿生效）"
	if v.PrevID != "" {
		prev = v.PrevID
	}
	reason := "无（首版没有更正原因）"
	if v.Reason != "" {
		reason = fmt.Sprintf("%q", v.Reason)
	}
	fmt.Printf("  历史[%d]: 版本号=%d，版本标识=%s，上一版本标识=%s，更正原因=%s\n",
		index, v.Number, v.ID, prev, reason)
	fmt.Printf("           正文=%q\n", v.Content)
}

func must(step string, err error) {
	if err != nil {
		die("%s失败: %v", step, err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "example: "+format+"\n", args...)
	os.Exit(1)
}
