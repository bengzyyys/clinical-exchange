// 命令 correction_conflict 演示更正遇到版本冲突后的完整处理过程。
//
// 现有说明只讲到 CorrectRecord 必须携带当前版本号、版本过期返回
// ErrConflict，却没有展示：拿着旧版本号提交被拒之后，使用者怎样查看
// 最新内容并再次更正。本示例围绕同一条合成诊断记录把这条路径走完：
//
//   - 两名不同的内部使用者（甲、乙）先各自取得该诊断第 1 版的信息；
//   - 甲先以版本号 1 更正成功，记录成为第 2 版；
//   - 乙仍用先前取得的版本号 1 提交另一份完整正文与非空原因，被
//     ErrConflict 明确拒绝；失败返回值是零值 Version，不是已保存的新版本；
//   - 核对正式记录仍为第 2 版：正文与原因保留甲第一次更正的结果，
//     历史只有两版，没有新增更正审计；
//   - 乙重新查看记录当前内容，确认第 2 版已有的变化后，以最新版本号 2
//     提交在第 2 版基础上补充的完整正文，成功产生第 3 版（第 3 版接在
//     第 2 版之后，前两版不被覆盖）；
//   - 最后对照“更正原因全为空白”：即使携带的是当前版本号，也返回
//     ErrInvalidArgument，同样不产生新版本或审计——它与 ErrConflict
//     是两类不同的失败。
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

// 三版正文与两次成功更正的原因。第 3 版正文必须完整包含第 2 版已经确认
// 的变化（胸片结论），再体现乙本次补充（血常规）——更正提交的始终是
// 完整正文，库不会把两人的修改自动合并。
const (
	contentV1 = "合成诊断：急性支气管炎 J20"
	contentV2 = "合成诊断：急性支气管炎 J20（已复核胸片，支持本诊断）"
	reasonV2  = "甲：复核胸片后补充诊断依据"

	// 乙在被拒那次提交中拟的正文与原因：基于手中的第 1 版写成，
	// 被 ErrConflict 拒绝后不应出现在任何历史版本中。
	contentBStale = "合成诊断：急性支气管炎 J20（血常规提示细菌感染）"
	reasonBStale  = "乙：根据血常规结果修订诊断措辞"

	contentV3 = "合成诊断：急性支气管炎 J20（已复核胸片，支持本诊断；补充血常规结果）"
	reasonV3  = "乙：在甲已确认的胸片结论之上补充血常规结果"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-correction-conflict-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctorA := clinical.InternalActor("doctor-a") // 内部使用者甲
	doctorB := clinical.InternalActor("doctor-b") // 内部使用者乙

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

	fmt.Println("准备完成: 本地存储、1 名合成患者、1 次就诊、1 条已生效诊断")
	fmt.Printf("记录标识 recordID=%s 在三个版本之间保持不变；第 1 版：版本标识=%s、整数版本号=%d、正文=%q\n",
		draft.ID, v1.ID, v1.Number, v1.Content)

	// ---- 两名内部使用者各自取得第 1 版信息（模拟各自开始工作前的查看）----
	baseA := mustCurrent(store, doctorA, patient.ID, encounter.ID, draft.ID, "使用者甲查看当前诊断")
	baseB := mustCurrent(store, doctorB, patient.ID, encounter.ID, draft.ID, "使用者乙查看当前诊断")
	bothAtV1 := baseA.Number == 1 && baseB.Number == 1 &&
		baseA.ID == v1.ID && baseB.ID == v1.ID &&
		baseA.Content == contentV1 && baseB.Content == contentV1
	fmt.Printf("两名使用者分别查看: 甲、乙取得的整数版本号均为 1、版本标识同为 %s、正文一致=%v\n",
		v1.ID, bothAtV1)

	// ---- 甲先以当前版本号 1 更正：成功，记录成为第 2 版 ----
	v2, err := store.CorrectRecord(doctorA, draft.ID, baseA.Number, contentV2, reasonV2)
	must("甲以版本号 1 提交第一次更正", err)
	fmt.Printf("甲的更正: 成功 -> 新版本标识=%s，整数版本号=%d，上一版本标识=%s（等于第 1 版标识）=%v，原因=%q\n",
		v2.ID, v2.Number, v2.PrevID, v2.PrevID == v1.ID, v2.Reason)

	// ---- 乙仍用先前取得的旧版本号 1 提交另一份正文与非空原因：被拒 ----
	stale, err := store.CorrectRecord(doctorB, draft.ID, baseB.Number, contentBStale, reasonBStale)
	switch {
	case err == nil:
		die("乙用旧版本号 1 更正应当被拒绝，却返回了成功版本 %+v", stale)
	case errors.Is(err, clinical.ErrConflict):
		// 预期失败：明确指出哪次被拒。
	default:
		die("乙用旧版本号 1 更正应返回 ErrConflict，实际得到 %v", err)
	}
	fmt.Printf("乙的更正（仍用先前取得的版本号 %d，提交另一份正文与非空原因）: 被拒绝（%v）\n",
		baseB.Number, err)
	zeroReturn := stale == clinical.Version{}
	fmt.Printf("  被拒调用返回的 Version 是零值（版本号=%d、版本标识=%q、上一版本标识=%q、正文=%q、原因=%q）=%v\n",
		stale.Number, stale.ID, stale.PrevID, stale.Content, stale.Reason, zeroReturn)
	fmt.Println("  该零值不是“第 3 版”，也不是任何已保存内容；本示例不会把它当作新版本继续使用")

	// ---- 核对正式记录：仍为第 2 版，乙拟提交的内容没有留下任何痕迹 ----
	history := mustHistory(store, doctorA, patient.ID, encounter.ID, draft.ID, "冲突后核对正式记录")
	current := *history.CurrentVersion
	officialStaysV2 := current.Number == 2 && current.ID == v2.ID &&
		current.Content == contentV2
	_, stalePersisted := findVersionContent(history.Versions, contentBStale)
	historyOnlyTwo := len(history.Versions) == 2 && !stalePersisted
	auditsAfterConflict := mustCorrectionAudits(store, doctorA, patient.ID, "冲突后查看更正审计")
	fmt.Printf("冲突后正式记录: 当前仍为第 2 版（版本标识=%s）=%v；正文与原因保留甲第一次更正的结果=%v\n",
		current.ID, officialStaysV2 && current.Content == contentV2,
		current.Content == contentV2)
	fmt.Printf("  历史仍只有两版（实际 %d 版）、乙拟提交的正文与原因均不在历史中: %v；更正审计 %d 条，没有新增乙的更正审计: %v\n",
		len(history.Versions), historyOnlyTwo,
		len(auditsAfterConflict), len(auditsAfterConflict) == 1)

	// ---- 乙重新查看记录的当前内容，确认第 2 版的变化后用最新版本号重提 ----
	refetched := mustCurrent(store, doctorB, patient.ID, encounter.ID, draft.ID,
		"使用者乙被拒后重新查看当前诊断")
	if refetched.Number != 2 || refetched.Content != contentV2 || refetched.Reason != reasonV2 {
		die("乙重新查看应得到第 2 版、甲更正后的正文与原因，实际版本号=%d 正文=%q 原因=%q",
			refetched.Number, refetched.Content, refetched.Reason)
	}
	fmt.Printf("乙重新查看: 当前整数版本号=%d（已不是手中的 1）、版本标识=%s、当前正文=%q、上一次更正原因=%q\n",
		refetched.Number, refetched.ID, refetched.Content, refetched.Reason)

	// 更正提交的是完整正文：库不会把乙先前拟的修改与第 2 版自动合并。
	// 乙确认保留第 2 版已确认的胸片结论，再补上本次的血常规结果，
	// 亲自拼出完整正文（contentV3）后，以重新查看得到的当前版本号 2 提交。
	if !strings.Contains(contentV3, "已复核胸片，支持本诊断") {
		die("第 3 版正文应当完整保留第 2 版已确认的变化")
	}
	v3, err := store.CorrectRecord(doctorB, draft.ID, refetched.Number, contentV3, reasonV3)
	if err != nil {
		die("乙以最新版本号 2 重新更正失败: %v", err)
	}
	fmt.Printf("乙重新确认后更正（以重新查看得到的版本号 %d 提交完整正文）: 成功 -> 新版本标识=%s，整数版本号=%d\n",
		refetched.Number, v3.ID, v3.Number)
	fmt.Printf("  上一版本标识=%s（等于第 2 版标识）=%v；第 3 版保留第 2 版已确认的胸片结论=%v，原因=%q\n",
		v3.PrevID, v3.PrevID == v2.ID, strings.Contains(v3.Content, "已复核胸片，支持本诊断"), v3.Reason)

	// ---- 对照另一类失败：更正原因全为空白 -> ErrInvalidArgument ----
	// 这次携带的版本号是当前版本号 3，因此不是版本冲突；被拒的唯一原因
	// 是参数不合法。它同样不产生新版本、不新增更正审计。
	auditsBeforeBlank := mustCorrectionAudits(store, doctorA, patient.ID, "空白原因试验前查看更正审计")
	blank, err := store.CorrectRecord(doctorB, draft.ID, v3.Number,
		"合成诊断：这版正文不应被保存", " \t ")
	switch {
	case err == nil:
		die("原因为空白的更正应当被拒绝，却返回了成功版本 %+v", blank)
	case errors.Is(err, clinical.ErrInvalidArgument):
		// 预期失败：参数不合法，而非版本冲突。
	case errors.Is(err, clinical.ErrConflict):
		die("携带当前版本号 %d 不应得到 ErrConflict", v3.Number)
	default:
		die("原因全为空白应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("原因全为空白的更正（携带的恰是当前版本号 %d）: 被拒绝（%v，不是 ErrConflict）；返回 Version 零值=%v\n",
		v3.Number, err, blank == clinical.Version{})

	history = mustHistory(store, doctorA, patient.ID, encounter.ID, draft.ID, "空白原因试验后核对正式记录")
	auditsAfterBlank := mustCorrectionAudits(store, doctorA, patient.ID, "空白原因试验后查看更正审计")
	noChange := history.CurrentVersion.Number == 3 &&
		history.CurrentVersion.ID == v3.ID &&
		history.CurrentVersion.Content == contentV3 &&
		len(history.Versions) == 3 &&
		len(auditsAfterBlank) == len(auditsBeforeBlank)
	fmt.Printf("  正式记录仍为第 3 版、历史仍为 3 版、更正审计仍为 %d 条=%v：参数失败同样不产生新版本或审计\n",
		len(auditsAfterBlank), noChange)

	// ---- 最终核对：打印三个版本与两次成功更正各自的审计 ----
	printVersionHistory(history, v1, v2, v3)
	printCorrectionAudits(auditsAfterBlank, draft.ID, doctorA.ID, doctorB.ID)
}

// mustCurrent 以内部查询 EncounterRecords 查看记录，返回其当前生效版本。
// 这是更正被拒后取得“最新版本号与当前正文”的正式途径。
func mustCurrent(store *clinical.Store, actor clinical.Actor, patientID, encounterID, recordID clinical.ID, step string) clinical.Version {
	h := mustHistory(store, actor, patientID, encounterID, recordID, step)
	if h.CurrentVersion == nil {
		die("%s：记录 %s 没有当前生效版本", step, recordID)
	}
	return *h.CurrentVersion
}

// mustHistory 查询某次就诊并按记录标识取回该记录的完整视图；查询失败或
// 记录缺失都明确指出失败的操作并停止后续步骤。
func mustHistory(store *clinical.Store, actor clinical.Actor, patientID, encounterID, recordID clinical.ID, step string) *clinical.RecordHistory {
	histories, err := store.EncounterRecords(actor, patientID, encounterID)
	must(step, err)
	h := findRecordHistory(histories, recordID)
	if h == nil {
		die("%s：查询结果中找不到记录 %s", step, recordID)
	}
	return h
}

func findRecordHistory(hs []clinical.RecordHistory, id clinical.ID) *clinical.RecordHistory {
	for i := range hs {
		if hs[i].Record.ID == id {
			return &hs[i]
		}
	}
	return nil
}

// findVersionContent 按完整正文在历史中定位版本；找不到返回 false。
func findVersionContent(vs []clinical.Version, content string) (clinical.Version, bool) {
	for _, v := range vs {
		if v.Content == content {
			return v, true
		}
	}
	return clinical.Version{}, false
}

// mustCorrectionAudits 取回该患者的审计并只保留更正事件，按发生先后排列。
func mustCorrectionAudits(store *clinical.Store, actor clinical.Actor, patientID clinical.ID, step string) []clinical.AuditEvent {
	events, err := store.AuditEvents(actor, patientID)
	must(step, err)
	corrected := make([]clinical.AuditEvent, 0, len(events))
	for _, e := range events {
		if e.Action == clinical.ActionCorrected {
			corrected = append(corrected, e)
		}
	}
	return corrected
}

// printVersionHistory 打印三版各自的正文、整数版本号、上一版本标识与更正
// 原因，并核对版本链：第 1 版无上一版与原因，第 2、3 版分别指向紧邻上一版。
func printVersionHistory(h *clinical.RecordHistory, v1, v2, v3 clinical.Version) {
	if len(h.Versions) != 3 {
		die("最终历史应有 3 个版本，实际为 %d 个", len(h.Versions))
	}
	vs := h.Versions
	if vs[0].ID != v1.ID || vs[1].ID != v2.ID || vs[2].ID != v3.ID {
		die("三个版本应按版本号旧到新为 v1、v2、v3，实际 %s、%s、%s",
			vs[0].ID, vs[1].ID, vs[2].ID)
	}
	if vs[0].PrevID != "" || vs[0].Reason != "" ||
		vs[1].PrevID != vs[0].ID || vs[2].PrevID != vs[1].ID {
		die("版本链指向有误: v1.PrevID=%q v2.PrevID=%q v3.PrevID=%q",
			vs[0].PrevID, vs[1].PrevID, vs[2].PrevID)
	}
	fmt.Println("\n=== 正式记录的三个版本（记录标识不变；第 3 版接在第 2 版之后，前两版未被覆盖）===")
	for i, v := range vs {
		prev := "无（首版由草稿生效）"
		if v.PrevID != "" {
			prev = string(v.PrevID)
		}
		reason := "无（首版没有更正原因）"
		if v.Reason != "" {
			reason = fmt.Sprintf("%q", v.Reason)
		}
		fmt.Printf("  版本[%d] 整数版本号=%d 版本标识=%s 上一版本标识=%s 更正原因=%s\n            正文=%q\n",
			i+1, v.Number, v.ID, prev, reason, v.Content)
	}
	fmt.Printf("版本链核对: %s(1) <- %s(2) <- %s(3)，第 1 版正文与第 2 版正文均原样保留\n",
		vs[0].ID, vs[1].ID, vs[2].ID)
}

// printCorrectionAudits 打印两次成功更正各自的审计：两次操作的操作者不同，
// 对象都是同一条记录；被 ErrConflict / ErrInvalidArgument 拒绝的提交不在内。
func printCorrectionAudits(audits []clinical.AuditEvent, recordID clinical.ID, actorA, actorB string) {
	if len(audits) != 2 {
		die("最终应有 2 条更正审计（两次成功更正各一条），实际 %d 条", len(audits))
	}
	if audits[0].ActorID != actorA || audits[1].ActorID != actorB {
		die("两条更正审计应依次属于甲（%s）与乙（%s），实际 %s、%s",
			actorA, actorB, audits[0].ActorID, audits[1].ActorID)
	}
	fmt.Println("\n=== 更正审计（仅供内部使用者按患者查看）===")
	for i, e := range audits {
		if e.ObjectID != recordID {
			die("更正审计 %d 的对象应为记录 %s，实际 %s", i+1, recordID, e.ObjectID)
		}
		fmt.Printf("  更正审计[%d]: 动作=%s，操作者=%s，对象类型=%s，对象（记录标识）=%s，发生时间=%v\n",
			i+1, e.Action, e.ActorID, e.ObjectType, e.ObjectID, e.OccurredAt.Format(time.RFC3339Nano))
	}
	fmt.Println("两次成功更正各有自己的审计；被拒的两次提交（ErrConflict、ErrInvalidArgument）均无审计")
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
