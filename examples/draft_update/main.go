// 命令 draft_update 演示内部使用者怎样修改一条尚未生效的诊断草稿，并区分
// “保存草稿”与“正式生效”对共享内容的不同影响。
//
// 现有说明已经讲过草稿可以修改，却没有把保存草稿（UpdateDraft）与生效
// （ActivateRecord）对内部记录、版本历史、审计与接收方可见内容的差别走一遍。
// 本示例用一名合成患者的一次就诊把这条路径走完：
//
//   - 该就诊下准备两条同类（诊断）记录：一条已经生效，另一条保持草稿；
//   - 为接收方建立覆盖这次就诊诊断的有效整类授权；
//   - 对目标草稿用 UpdateDraft 替换完整正文（新正文含中文、换行与有意保留的
//     首尾空格）：记录标识不变、没有当前生效版本、历史版本仍为空、不新增审计；
//     接收方在有效授权下修改前后都只读到已经生效的那一条；
//   - 目标仍是草稿时，提交仅含空格/换行的正文、夹带非法 UTF-8 字节的正文，
//     都返回 ErrInvalidArgument，先前成功保存的草稿原样保留、不会部分保存；
//   - 随后把修改后的草稿生效：第一个正式版本号为 1，采用最后一次成功保存的
//     完整正文，旧草稿文字不会成为历史版本；接收方此时才读到目标诊断；
//   - 生效后再用合法新正文走 UpdateDraft 得到 ErrActive，当前内容与版本历史
//     不变；后续修改应使用已有的更正功能 CorrectRecord（示例最后演示一次）。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/draft_update
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 一次就诊中两条同类诊断的正文，以及目标草稿在各阶段提交的内容。
const (
	// activeContent 是先建好并生效的第一条诊断，整个示例期间都不是修改目标。
	activeContent = "合成诊断一：原发性高血压 I10"

	// originalDraft 是目标诊断的最初草稿正文；它只会停留在草稿阶段，
	// 生效前被一次成功的 UpdateDraft 整体替换，不会成为任何历史版本。
	originalDraft = "合成诊断二：疑似高脂血症 E78.5（待复核）"

	// revisedDraft 是对目标草稿唯一一次成功的修改：含中文、换行，以及有意
	// 保留的首尾空格。UpdateDraft 替换的是完整正文，这些字符按原样保存。
	revisedDraft = "  合成诊断二：混合性高脂血症 E78.4\n低密度脂蛋白 3.6 mmol/L，建议饮食控制并两周后复查  "

	// blankAttempt 只含空格、制表与换行：去掉空白后为空，必须被拒绝。
	blankAttempt = "  \n\t "

	// invalidAttempt 在合法中文之间夹带一个非法 UTF-8 字节：不能替换、
	// 截断或只保存前半段，必须整体拒绝。
	invalidAttempt = "合成诊断二：夹带坏字节 \xff 的正文"

	// 生效之后若还要改内容，必须走更正：带当前版本号与非空、合法 UTF-8
	// 的更正原因提交完整正文。
	correctedContent = "合成诊断二：混合性高脂血症 E78.4（复查后低密度脂蛋白下降）"
	correctionReason = "复查血脂下降，补充确诊结论"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-draft-update-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备：一名合成患者、一次就诊、两条同类诊断（一生效、一草稿）----
	patient, err := store.RegisterPatient(doctor, "合成患者寅")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	// 记录一：建好后立即生效，整个示例期间不受目标草稿操作影响。
	activeDiag, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, activeContent)
	must("创建第一条诊断草稿", err)
	activeV1, err := store.ActivateRecord(doctor, activeDiag.ID)
	must("生效第一条诊断", err)

	// 记录二（目标）：保存为草稿后保持草稿状态，等待下面修改与生效。
	target, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, originalDraft)
	must("创建目标诊断草稿（保持草稿状态）", err)

	// 为接收方建立覆盖这次就诊“诊断”整类的有效授权，时间窗
	// [now, now+24h)。整类授权覆盖该就诊下全部已生效诊断，但不含草稿。
	now := time.Now()
	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must("为接收方建立覆盖本次就诊诊断的有效整类授权", err)

	fmt.Println("准备完成: 1 名合成患者、1 次就诊；同一次就诊有两条同类诊断：")
	fmt.Printf("  记录一（已生效，记录标识=%s）: %q\n", activeDiag.ID, activeContent)
	fmt.Printf("  记录二（草稿，记录标识=%s）: %q\n", target.ID, originalDraft)
	fmt.Printf("  接收方 %s 持有覆盖本次就诊诊断的有效整类授权（授权 %s）\n",
		receiver.ID, grant.ID)

	// ---- 修改前：内部就诊记录视图 ----
	fmt.Println("\n========== 修改前：内部就诊记录视图（EncounterRecords）==========")
	before := mustEncounterRecords(store, doctor, patient.ID, encounter.ID)
	printEncounterView("修改前", before)

	activeBefore := findHistory(before, activeDiag.ID)
	targetBefore := findHistory(before, target.ID)
	if activeBefore == nil || targetBefore == nil {
		die("就诊视图应同时包含两条诊断，实际 %v", recordIDList(before))
	}
	if !targetBefore.HasDraft || targetBefore.DraftContent != originalDraft ||
		targetBefore.CurrentVersion != nil || len(targetBefore.Versions) != 0 {
		die("修改前目标应为原始草稿、无当前生效版本与历史版本，实际 %+v", targetBefore)
	}
	if activeBefore.CurrentVersion == nil || activeBefore.CurrentVersion.ID != activeV1.ID ||
		len(activeBefore.Versions) != 1 {
		die("修改前第一条诊断应为已生效的第 1 版，实际 %+v", activeBefore)
	}

	// ---- 修改前：接收方在有效授权下读取 ----
	readBefore := mustRead(store, receiver, patient.ID, encounter.ID)
	fmt.Println("========== 修改前：接收方读取（Read）==========")
	printReadView("修改前", readBefore)
	if len(readBefore.Records) != 1 || readBefore.Records[0].RecordID != activeDiag.ID ||
		readBefore.Records[0].Version != 1 || readBefore.Records[0].Content != activeContent {
		die("修改前接收方应只读到已生效的第一条诊断，实际 %+v", readBefore.Records)
	}
	if readContains(readBefore, target.ID) {
		die("修改前接收方结果不得包含目标草稿的标识或正文，实际 %+v", readBefore.Records)
	}

	// ---- 保存草稿：UpdateDraft 用完整新正文替换草稿 ----
	// 成功保存只改草稿正文：记录标识不变、不产生版本、不产生审计事件，
	// 也不能把它说成对生效记录的更正。
	updated, err := store.UpdateDraft(doctor, target.ID, revisedDraft)
	must("UpdateDraft 保存修改后的草稿", err)
	if updated.ID != target.ID {
		die("保存草稿不得更换记录标识：保存前=%s 保存后=%s", target.ID, updated.ID)
	}
	if updated.DraftContent != revisedDraft || !updated.HasDraft ||
		updated.CurrentVersionID != "" || len(updated.Versions) != 0 {
		die("UpdateDraft 返回记录异常: %+v", updated)
	}
	fmt.Println("\n========== 保存草稿：UpdateDraft（成功）==========")
	fmt.Printf("目标记录标识不变: %s == %s = true\n", target.ID, updated.ID)
	fmt.Printf("草稿正文已整体替换为提交的新内容（%%q 原样展示，首尾空格与 \\n 可辨认）:\n  %q\n",
		updated.DraftContent)
	fmt.Printf("逐字与提交一致=%v；仍是草稿=%v；当前生效版本标识=%q（空）；历史版本数=%d\n",
		updated.DraftContent == revisedDraft, updated.HasDraft,
		updated.CurrentVersionID, len(updated.Versions))

	// ---- 修改后：内部就诊记录视图 ----
	fmt.Println("\n========== 修改后：内部就诊记录视图（EncounterRecords）==========")
	after := mustEncounterRecords(store, doctor, patient.ID, encounter.ID)
	printEncounterView("修改后", after)

	activeAfter := findHistory(after, activeDiag.ID)
	targetAfter := findHistory(after, target.ID)
	if !targetAfter.HasDraft || targetAfter.DraftContent != revisedDraft ||
		targetAfter.CurrentVersion != nil || len(targetAfter.Versions) != 0 ||
		targetAfter.Record.ID != target.ID ||
		targetAfter.Record.EncounterID != encounter.ID ||
		targetAfter.Record.Category != clinical.Diagnosis {
		die("保存后目标应为同一标识、新正文的草稿，无版本，实际 %+v", targetAfter)
	}
	if !historyEqual(activeAfter, activeBefore) {
		die("保存草稿不得影响另一条已生效诊断：修改前 %+v 修改后 %+v",
			activeBefore, activeAfter)
	}
	fmt.Println("核对: 目标记录标识/患者/就诊/类别均不变；草稿正文即原样提交的新内容；")
	fmt.Printf("  目标仍无当前生效版本、历史版本数=%d；另一条已生效诊断与修改前完全一致=%v\n",
		len(targetAfter.Versions), historyEqual(activeAfter, activeBefore))

	// 保存草稿不新增版本或审计事件：审计集合与准备完成时完全相同。
	auditsAfterSave := mustAudits(store, doctor, patient.ID)
	fmt.Printf("审计核对: 保存草稿后审计事件数=%d（动作计数 %v）——保存草稿不产生审计事件\n",
		len(auditsAfterSave), auditCounts(auditsAfterSave))
	if got := auditCounts(auditsAfterSave); got[clinical.ActionActivated] != 1 ||
		got[clinical.ActionGranted] != 1 || got[clinical.ActionCorrected] != 0 {
		die("保存草稿后只应有第一条诊断的生效审计与授权创建审计各 1 条，实际 %v", got)
	}

	// ---- 修改后：接收方仍只读到已经生效的那一条，结果与修改前逐字相同 ----
	readAfterSave := mustRead(store, receiver, patient.ID, encounter.ID)
	fmt.Println("\n========== 修改后：接收方读取（Read）==========")
	printReadView("修改后", readAfterSave)
	if !readResultEqual(readAfterSave, readBefore) {
		die("保存草稿前后接收方结果应逐字相同：修改前 %+v 修改后 %+v",
			readBefore, readAfterSave)
	}
	if readContains(readAfterSave, target.ID) ||
		strings.Contains(readAfterSave.Records[0].Content, "高脂血症") {
		die("接收方不得得到目标草稿的正文或标识，实际 %+v", readAfterSave.Records)
	}
	fmt.Println("核对: 接收方读到的内容与修改前逐字相同（仍只有第一条诊断的第 1 版）；")
	fmt.Println("  目标草稿的正文与标识都不在结果中——保存草稿不改变任何共享内容")

	// ---- 目标仍是草稿时，预期被拒的修改一：正文仅含空格与换行 ----
	rejected, err := store.UpdateDraft(doctor, target.ID, blankAttempt)
	switch {
	case err == nil:
		die("全空白正文的草稿修改应当被拒绝，却返回了记录 %+v", rejected)
	case errors.Is(err, clinical.ErrInvalidArgument):
		fmt.Printf("\n【拒绝修改一·全空白】提交 %q: %v\n", blankAttempt, err)
		fmt.Println("  错误类型=ErrInvalidArgument；失败返回的是空记录，不当作保存结果使用")
	default:
		die("全空白正文应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	assertDraftUnchanged(store, doctor, patient.ID, encounter.ID, target.ID,
		revisedDraft, "全空白修改被拒后")

	// ---- 预期被拒的修改二：正文夹带非法 UTF-8 字节 ----
	rejected, err = store.UpdateDraft(doctor, target.ID, invalidAttempt)
	switch {
	case err == nil:
		die("夹带非法 UTF-8 的草稿修改应当被拒绝，却返回了记录 %+v", rejected)
	case errors.Is(err, clinical.ErrInvalidArgument):
		fmt.Printf("\n【拒绝修改二·非法 UTF-8】提交 %q: %v\n", invalidAttempt, err)
		puts("  错误类型=ErrInvalidArgument；不替换坏字节、不截断、不只保存前半段")
	default:
		die("非法 UTF-8 正文应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	assertDraftUnchanged(store, doctor, patient.ID, encounter.ID, target.ID,
		revisedDraft, "非法 UTF-8 修改被拒后")

	// ---- 正式生效：把最后一次成功保存的草稿固化为第 1 版 ----
	v1, err := store.ActivateRecord(doctor, target.ID)
	must("ActivateRecord 使修改后的草稿生效", err)
	if v1.Number != 1 || v1.RecordID != target.ID || v1.Content != revisedDraft ||
		v1.PrevID != "" || v1.Reason != "" {
		die("生效结果异常（应为第 1 版、正文为最后一次成功保存的完整正文、无前版与原因）: %+v", v1)
	}
	fmt.Println("\n========== 正式生效：ActivateRecord ==========")
	fmt.Printf("目标诊断的第一个正式版本: 版本号=%d，版本标识=%s，记录标识=%s\n",
		v1.Number, v1.ID, v1.RecordID)
	fmt.Printf("第 1 版正文采用最后一次成功保存的完整内容（%%q）:\n  %q\n  与提交逐字一致=%v\n",
		v1.Content, v1.Content == revisedDraft)
	fmt.Println("首版没有上一版本标识、没有更正原因；旧草稿文字不会成为历史版本")

	activatedView := mustEncounterRecords(store, doctor, patient.ID, encounter.ID)
	fmt.Println("\n---------- 生效后：内部就诊记录视图 ----------")
	printEncounterView("生效后", activatedView)
	targetActive := findHistory(activatedView, target.ID)
	if targetActive.HasDraft || targetActive.DraftContent != "" ||
		targetActive.CurrentVersion == nil || targetActive.CurrentVersion.ID != v1.ID ||
		len(targetActive.Versions) != 1 {
		die("生效后目标应无草稿、恰有第 1 版，实际 %+v", targetActive)
	}
	for _, v := range targetActive.Versions {
		if strings.Contains(v.Content, originalDraft) {
			die("旧草稿文字不得成为历史版本，实际版本 %+v", v)
		}
	}
	fmt.Printf("核对: 目标草稿标记=%v、草稿正文=%q（已清空）；当前生效版本号=%d；历史版本数=%d\n",
		targetActive.HasDraft, targetActive.DraftContent,
		targetActive.CurrentVersion.Number, len(targetActive.Versions))
	fmt.Printf("  历史中没有旧草稿文字 %q=%v\n", originalDraft,
		!strings.Contains(targetActive.Versions[0].Content, originalDraft))

	// 生效本身是审计事件；此前所有草稿保存（含被拒的两次）都没有留下审计。
	auditsAfterActivate := mustAudits(store, doctor, patient.ID)
	fmt.Printf("审计核对: 生效后事件数=%d（动作计数 %v）——本次生效新增 1 条 activated\n",
		len(auditsAfterActivate), auditCounts(auditsAfterActivate))

	// ---- 生效后：接收方此时才读到目标诊断 ----
	readAfterActivate := mustRead(store, receiver, patient.ID, encounter.ID)
	fmt.Println("\n---------- 生效后：接收方读取 ----------")
	printReadView("生效后", readAfterActivate)
	if len(readAfterActivate.Records) != 2 {
		die("生效后接收方应读到两条诊断，实际 %d 条", len(readAfterActivate.Records))
	}
	gotTarget := findEffective(readAfterActivate, target.ID)
	if gotTarget == nil || gotTarget.Version != 1 || gotTarget.VersionID != v1.ID ||
		gotTarget.Content != revisedDraft {
		die("接收方此时应读到目标诊断的第 1 版（最后保存的完整正文），实际 %+v", gotTarget)
	}
	gotActive := findEffective(readAfterActivate, activeDiag.ID)
	if gotActive == nil || gotActive.Version != 1 || gotActive.Content != activeContent {
		die("第一条诊断对接收方应保持不变，实际 %+v", gotActive)
	}
	fmt.Println("核对: 接收方此时才读到目标诊断（第 1 版，正文即最后保存的完整内容）；")
	fmt.Println("  第一条已生效诊断不受影响，仍是原来的第 1 版")

	// ---- 生效后再走草稿路径：合法新正文也返回 ErrActive ----
	rejected, err = store.UpdateDraft(doctor, target.ID, "生效后试图按草稿覆盖")
	switch {
	case err == nil:
		die("已生效记录再走 UpdateDraft 应当被拒绝，却返回了记录 %+v", rejected)
	case errors.Is(err, clinical.ErrActive):
		fmt.Printf("\n【拒绝修改·已生效】对已有生效版本的记录调用 UpdateDraft: %v\n", err)
		fmt.Println("  错误类型=ErrActive；生效记录不能再按草稿方式直接覆盖或删除")
	default:
		die("已生效记录走 UpdateDraft 应返回 ErrActive，实际得到 %v", err)
	}
	unchangedView := mustEncounterRecords(store, doctor, patient.ID, encounter.ID)
	targetUnchanged := findHistory(unchangedView, target.ID)
	if targetUnchanged.CurrentVersion == nil ||
		targetUnchanged.CurrentVersion.ID != v1.ID ||
		targetUnchanged.CurrentVersion.Content != revisedDraft ||
		len(targetUnchanged.Versions) != 1 {
		die("ErrActive 后当前内容与版本历史应保持不变，实际 %+v", targetUnchanged)
	}
	fmt.Printf("拒绝后核对: 当前内容仍为第 1 版（版本标识=%s、正文未变）=%v；历史版本数=%d（未增加）\n",
		v1.ID,
		targetUnchanged.CurrentVersion.ID == v1.ID &&
			targetUnchanged.CurrentVersion.Content == revisedDraft,
		len(targetUnchanged.Versions))

	// ---- 后续修改应使用已有的更正功能：带当前版本号与非空原因提交完整正文 ----
	v2, err := store.CorrectRecord(doctor, target.ID, v1.Number,
		correctedContent, correctionReason)
	must("用 CorrectRecord 更正已生效的目标诊断", err)
	finalView := mustEncounterRecords(store, doctor, patient.ID, encounter.ID)
	targetFinal := findHistory(finalView, target.ID)
	if v2.Number != 2 || v2.PrevID != v1.ID || len(targetFinal.Versions) != 2 ||
		targetFinal.CurrentVersion.ID != v2.ID {
		die("更正后应为第 2 版且保留第 1 版，实际 v2=%+v 历史=%+v", v2, targetFinal)
	}
	fmt.Println("\n========== 后续修改：使用已有更正功能 CorrectRecord ==========")
	fmt.Printf("CorrectRecord（当前版本号=%d，非空原因）成功: 产生第 %d 版，版本标识=%s，上一版本标识=%s\n",
		v1.Number, v2.Number, v2.ID, v2.PrevID)
	fmt.Printf("  新正文=%q\n  更正原因=%q\n", v2.Content, v2.Reason)
	fmt.Printf("核对: 版本历史现有 %d 版（第 1 版原样保留），当前版本为第 %d 版；"+
		"草稿阶段的新旧文字都不占版本\n",
		len(targetFinal.Versions), targetFinal.CurrentVersion.Number)
	fmt.Println("结论: 草稿阶段用 UpdateDraft 反复保存（不生效、不共享、不进版本与审计）；")
	fmt.Println("  生效之后只能用 CorrectRecord 更正，每一次更正都是带原因的新版本。")
}

// ---- 以下为示例辅助函数：查询、展示与状态核对 ----

// mustEncounterRecords 由内部使用者查询一次就诊的完整记录视图。
func mustEncounterRecords(store *clinical.Store, actor clinical.Actor,
	patientID, encounterID clinical.ID,
) []clinical.RecordHistory {
	rows, err := store.EncounterRecords(actor, patientID, encounterID)
	must("内部使用者查询就诊记录", err)
	return rows
}

// mustRead 由接收方读取某次就诊某类别的当前生效记录。
func mustRead(store *clinical.Store, actor clinical.Actor,
	patientID, encounterID clinical.ID,
) clinical.ReadResult {
	res, err := store.Read(actor, patientID, encounterID, clinical.Diagnosis)
	must("接收方读取本次就诊诊断", err)
	return res
}

// mustAudits 取回该患者的全部审计事件。
func mustAudits(store *clinical.Store, actor clinical.Actor, patientID clinical.ID) []clinical.AuditEvent {
	events, err := store.AuditEvents(actor, patientID)
	must("查看审计事件", err)
	return events
}

// assertDraftUnchanged 核对一次被拒修改之后：目标仍是同一条草稿，草稿正文
// 仍是此前成功保存的内容，没有当前生效版本与历史版本，审计也不增加。
func assertDraftUnchanged(store *clinical.Store, actor clinical.Actor,
	patientID, encounterID, recordID clinical.ID, wantDraft, stage string,
) {
	rows := mustEncounterRecords(store, actor, patientID, encounterID)
	h := findHistory(rows, recordID)
	if h == nil || !h.HasDraft || h.DraftContent != wantDraft ||
		h.CurrentVersion != nil || len(h.Versions) != 0 || h.Record.ID != recordID {
		die("%s目标草稿应保持拒绝前状态（标识不变、正文=%q、无版本），实际 %+v",
			stage, wantDraft, h)
	}
	fmt.Printf("  %s核对: 先前成功保存的草稿仍保留（正文=%q），记录标识不变、历史版本数=%d\n",
		stage, h.DraftContent, len(h.Versions))
}

// printEncounterView 打印内部就诊记录视图中的每条记录（按记录标识升序，
// 与查询返回顺序一致），区分草稿形态与生效形态。
func printEncounterView(stage string, rows []clinical.RecordHistory) {
	fmt.Printf("[%s] 就诊记录数=%d（按记录标识升序）\n", stage, len(rows))
	for _, h := range rows {
		shape := "草稿"
		if h.CurrentVersion != nil {
			shape = "已生效"
		}
		fmt.Printf("  - 记录标识=%s，类别=%s，形态=%s，草稿标记 HasDraft=%v\n",
			h.Record.ID, h.Record.Category, shape, h.HasDraft)
		fmt.Printf("    草稿正文=%q\n", h.DraftContent)
		if h.CurrentVersion == nil {
			fmt.Printf("    当前生效版本: 无（CurrentVersion 为 nil）；历史版本数=%d\n", len(h.Versions))
			continue
		}
		fmt.Printf("    当前生效版本: 版本号=%d，版本标识=%s，正文=%q\n",
			h.CurrentVersion.Number, h.CurrentVersion.ID, h.CurrentVersion.Content)
		fmt.Printf("    历史版本数=%d", len(h.Versions))
		for i, v := range h.Versions {
			prev := "无"
			if v.PrevID != "" {
				prev = string(v.PrevID)
			}
			reason := "无"
			if v.Reason != "" {
				reason = fmt.Sprintf("%q", v.Reason)
			}
			fmt.Printf("\n      历史[%d]: 版本号=%d，版本标识=%s，上一版本标识=%s，更正原因=%s，正文=%q",
				i, v.Number, v.ID, prev, reason, v.Content)
		}
		fmt.Println()
	}
}

// printReadView 打印接收方读取结果。为输出稳定，展示前按记录标识排序
// （不改动返回切片本身）。
func printReadView(stage string, res clinical.ReadResult) {
	ids := make([]clinical.EffectiveRecord, len(res.Records))
	copy(ids, res.Records)
	sort.Slice(ids, func(i, j int) bool { return ids[i].RecordID < ids[j].RecordID })
	fmt.Printf("[%s] 接收方可见记录数=%d\n", stage, len(ids))
	for _, r := range ids {
		fmt.Printf("  - 记录标识=%s，版本号=%d，版本标识=%s，正文=%q\n",
			r.RecordID, r.Version, r.VersionID, r.Content)
	}
}

func findHistory(rows []clinical.RecordHistory, id clinical.ID) *clinical.RecordHistory {
	for i := range rows {
		if rows[i].Record.ID == id {
			return &rows[i]
		}
	}
	return nil
}

func findEffective(res clinical.ReadResult, id clinical.ID) *clinical.EffectiveRecord {
	for i := range res.Records {
		if res.Records[i].RecordID == id {
			return &res.Records[i]
		}
	}
	return nil
}

func readContains(res clinical.ReadResult, id clinical.ID) bool {
	return findEffective(res, id) != nil
}

// readResultEqual 比较两次接收方读取是否逐字一致（按记录标识归并后比较）。
func readResultEqual(a, b clinical.ReadResult) bool {
	if len(a.Records) != len(b.Records) {
		return false
	}
	ma, mb := map[clinical.ID]clinical.EffectiveRecord{}, map[clinical.ID]clinical.EffectiveRecord{}
	for _, r := range a.Records {
		ma[r.RecordID] = r
	}
	for _, r := range b.Records {
		mb[r.RecordID] = r
	}
	if len(ma) != len(mb) {
		return false
	}
	for id, x := range ma {
		y, ok := mb[id]
		if !ok || x != y {
			return false
		}
	}
	return true
}

// historyEqual 比较两条记录视图的关键字段（草稿、当前版本与完整历史）。
func historyEqual(a, b *clinical.RecordHistory) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Record.ID == b.Record.ID &&
		a.HasDraft == b.HasDraft && a.DraftContent == b.DraftContent &&
		versionsEqual(a.Versions, b.Versions) &&
		((a.CurrentVersion == nil && b.CurrentVersion == nil) ||
			(a.CurrentVersion != nil && b.CurrentVersion != nil &&
				*a.CurrentVersion == *b.CurrentVersion))
}

func versionsEqual(a, b []clinical.Version) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func auditCounts(events []clinical.AuditEvent) map[string]int {
	out := map[string]int{}
	for _, e := range events {
		out[e.Action]++
	}
	return out
}

func recordIDList(rows []clinical.RecordHistory) []clinical.ID {
	ids := make([]clinical.ID, len(rows))
	for i, h := range rows {
		ids[i] = h.Record.ID
	}
	return ids
}

func puts(msg string) { fmt.Println(msg) }

func must(step string, err error) {
	if err != nil {
		die("%s失败: %v", step, err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "example: "+format+"\n", args...)
	os.Exit(1)
}
