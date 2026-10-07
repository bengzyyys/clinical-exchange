// 命令 patient_audit 演示内部使用者如何用 AuditEvents 按患者单独查看审计
// 历史：只回答“谁在什么时候对哪条记录做了什么”，不需要先取得包含草稿、
// 病历正文与更正原因的完整档案。
//
// 示例围绕一名合成患者的一条诊断走完三次成功操作：
//   - 第一名内部使用者把诊断草稿生效；
//   - 另一名内部使用者更正这条诊断（生效与更正指向同一条记录）；
//   - 随后由内部使用者停用患者（停用事件指向患者档案本身）。
//
// 停用后查询该患者的审计，结果应逐次对应这三次操作：区分操作人、动作、
// 对象类型、对象标识与发生时间，并展示每条事件自己的标识及所属患者。
// 生效事件与更正事件的对象标识都是同一条记录标识（记录不因更正换标识），
// 而两条事件各有自己的事件标识——事件标识与记录标识不能混用。
//
// 示例同时交代：
//   - 查询范围是该患者的全部审计，不只查询者本人做过的操作（生效与更正
//     由两名不同的内部使用者完成，两人都能看到对方的事件）；
//   - 中间穿插另一名合成患者的一次生效操作，目标患者的结果只含自己的事件；
//   - 事件按实际操作先后返回，两次操作即使发生时间相同也保留先后顺序，
//     事件标识只用于区分事件，不表示操作先后；
//   - 患者停用后仍能看到原有历史与最后的停用事件；查询本身不新增审计；
//   - 审计事件不含病历正文与更正原因：需要这些内容仍用已有的完整历史
//     查询（EncounterRecords / Chart），一条更正事件不能代替完整更正记录；
//   - 权限与失败：接收方即使持有该患者诊断的有效授权，调用本内部查询也
//     得到 ErrAccessDenied 且没有事件；内部使用者查询不存在的患者得到
//     ErrNotFound 且没有事件；现有患者尚未产生审计时成功返回空清单。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/patient_audit
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-patient-audit-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	// 注入固定时钟，纯粹为了让示例能稳定演示“两次操作时间相同也保留先后”；
	// 生产环境使用默认实时时钟，顺序规则完全相同。
	clock := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	store, err := clinical.Open(dir, clinical.WithClock(func() time.Time { return clock }))
	must("打开本地存储", err)
	defer store.Close()

	// 两名不同的内部使用者：一人让草稿生效，另一人更正；审计要能区分操作人。
	doctorA := clinical.InternalActor("doctor-a")
	doctorB := clinical.InternalActor("doctor-b")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备目标患者：一次就诊与一条诊断草稿（草稿本身不留审计）----
	patient, err := store.RegisterPatient(doctorA, "合成患者癸")
	must("登记目标合成患者", err)
	encounter, err := store.AddEncounter(doctorA, patient.ID, clock)
	must("登记就诊", err)
	draft, err := store.CreateDraft(doctorA, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断：急性上呼吸道感染 J06")
	must("创建诊断草稿", err)
	fmt.Println("准备完成: 目标患者与诊断草稿已就绪（登记患者、就诊、草稿都不产生审计事件）")

	// ---- 成功操作一：doctor-a 把草稿生效为第 1 版 ----
	v1, err := store.ActivateRecord(doctorA, draft.ID)
	must("doctor-a 生效诊断草稿", err)
	activatedAt := v1.CreatedAt

	// ---- 成功操作二：doctor-b 更正同一条诊断 ----
	// 时钟不前进：刻意让更正事件与生效事件的发生时间相同，用来演示
	// “时间相同也保留真实操作先后”。
	v2, err := store.CorrectRecord(doctorB, draft.ID, v1.Number,
		"合成诊断：急性上呼吸道感染 J06（复核确认）", "复核后补充确认依据")
	must("doctor-b 更正诊断", err)
	correctedAt := v2.CreatedAt

	// ---- 中间穿插：另一名合成患者的一次生效操作，不得混入目标患者的审计 ----
	other, err := store.RegisterPatient(doctorA, "合成患者子")
	must("登记另一名合成患者", err)
	otherEnc, err := store.AddEncounter(doctorA, other.ID, clock)
	must("登记另一名患者的就诊", err)
	otherDraft, err := store.CreateDraft(doctorA, other.ID, otherEnc.ID,
		clinical.Diagnosis, "合成诊断：另一名患者的感冒 J00")
	must("创建另一名患者的诊断草稿", err)
	_, err = store.ActivateRecord(doctorA, otherDraft.ID)
	must("生效另一名患者的诊断", err)

	fmt.Printf("三次写入已完成: 生效（doctor-a）、更正（doctor-b）时间相同=%v；"+
		"另有一名合成患者的一次生效操作（记录 %s，属于患者 %s）\n",
		correctedAt.Equal(activatedAt), otherDraft.ID, other.ID)

	// ---- 停用前查询：只有生效、更正两条，另一名患者的事件不混入 ----
	eventsBefore := mustAudit(store, doctorB, patient.ID, "停用前查询目标患者审计")
	if len(eventsBefore) != 2 {
		die("停用前目标患者应有 2 条审计（生效、更正），实际得到 %d 条: %+v",
			len(eventsBefore), eventsBefore)
	}
	fmt.Printf("\n停用前查询（查询者是 doctor-b，但范围是该患者全部审计，不只自己做过的操作）: %d 条\n",
		len(eventsBefore))
	printAuditEvent(0, eventsBefore[0])
	printAuditEvent(1, eventsBefore[1])

	// 核对前两条事件：动作、操作人、对象类型、对象标识与时间逐字段对应。
	activated := eventsBefore[0]
	corrected := eventsBefore[1]
	if activated.Action != clinical.ActionActivated || activated.ActorID != doctorA.ID ||
		activated.ObjectType != "record" || activated.ObjectID != draft.ID ||
		activated.PatientID != patient.ID || !activated.OccurredAt.Equal(activatedAt) {
		die("生效事件字段不符: %+v", activated)
	}
	if corrected.Action != clinical.ActionCorrected || corrected.ActorID != doctorB.ID ||
		corrected.ObjectType != "record" || corrected.ObjectID != draft.ID ||
		corrected.PatientID != patient.ID || !corrected.OccurredAt.Equal(correctedAt) {
		die("更正事件字段不符: %+v", corrected)
	}
	fmt.Printf("核对: 生效与更正事件的对象标识都是同一条记录 %s（记录不因更正换标识）=%v；"+
		"两条事件各有自己的事件标识（%s、%s），事件标识与记录标识互不相混=%v\n",
		draft.ID, activated.ObjectID == corrected.ObjectID,
		activated.ID, corrected.ID,
		activated.ID != draft.ID && corrected.ID != draft.ID && activated.ID != corrected.ID)
	fmt.Printf("核对: 两条事件时间相同=%v，仍按真实操作先后返回（先生效、后更正）；"+
		"先后由返回位置体现，事件标识本身不编码先后\n",
		activated.OccurredAt.Equal(corrected.OccurredAt))

	// 审计事件里没有正文与更正原因字段。
	fmt.Printf("审计内容边界: 事件只携带操作人/动作/对象/时间与各自身份标识；"+
		"生效事件不含病历正文（第 1 版正文 %q 只在完整历史里），"+
		"更正事件不含更正原因（%q 只在完整历史里）\n",
		v1.Content, v2.Reason)
	fmt.Println("需要正文与更正原因时，仍使用已有的完整历史查询 EncounterRecords/Chart；" +
		"一条更正审计事件不能代替完整更正记录（旧正文、版本链与原因都不在审计里）")

	// ---- 成功操作三：停用患者；停用事件指向患者档案本身 ----
	clock = clock.Add(time.Hour)
	must("停用目标患者", store.DeactivatePatient(doctorA, patient.ID))
	deactivatedAt := clock

	// ---- 停用后再次查询：原有两条历史仍在，末尾是停用事件 ----
	eventsAfter := mustAudit(store, doctorB, patient.ID, "停用后查询目标患者审计")
	if len(eventsAfter) != 3 {
		die("停用后目标患者应有 3 条审计（生效、更正、停用），实际得到 %d 条: %+v",
			len(eventsAfter), eventsAfter)
	}
	fmt.Printf("\n患者停用后查询（停用不清空历史，内部使用者仍可查看）: %d 条\n", len(eventsAfter))
	for i, ev := range eventsAfter {
		printAuditEvent(i, ev)
	}

	deactivated := eventsAfter[2]
	if deactivated.Action != clinical.ActionDeactivated || deactivated.ActorID != doctorA.ID ||
		deactivated.ObjectType != "patient" || deactivated.ObjectID != patient.ID ||
		deactivated.PatientID != patient.ID || !deactivated.OccurredAt.Equal(deactivatedAt) {
		die("停用事件字段不符: %+v", deactivated)
	}
	if eventsAfter[0] != activated || eventsAfter[1] != corrected {
		die("停用后原有事件应原样保留在原位，实际 %+v", eventsAfter[:2])
	}
	fmt.Printf("核对: 停用事件对象类型=%q、对象标识为患者档案 %s（与两条记录事件的对象类型 %q 区分）；"+
		"原有 2 条历史原样保留，停用事件在末尾\n",
		deactivated.ObjectType, deactivated.ObjectID, activated.ObjectType)

	// 每条事件都带自己的事件标识与所属患者，且同患者内事件标识互不重复。
	seen := map[clinical.ID]bool{}
	for _, ev := range eventsAfter {
		if ev.ID == "" || seen[ev.ID] {
			die("事件标识缺失或重复: %q", ev.ID)
		}
		seen[ev.ID] = true
		if ev.PatientID != patient.ID {
			die("目标患者的审计里混入了其他患者的事件: %+v", ev)
		}
	}

	// 查询本身是只读的：再查一次，条数与内容不变，不新增审计事件。
	eventsAgain := mustAudit(store, doctorA, patient.ID, "重复查询确认只读")
	if len(eventsAgain) != len(eventsAfter) {
		die("重复查询不应改变事件数: %d != %d", len(eventsAgain), len(eventsAfter))
	}
	for i := range eventsAgain {
		if eventsAgain[i] != eventsAfter[i] {
			die("重复查询结果与上次不一致: %+v != %+v", eventsAgain[i], eventsAfter[i])
		}
	}
	fmt.Printf("只读核对: 再次查询仍为 %d 条且逐字段一致=%v；查询本身不新增审计事件\n",
		len(eventsAgain), true)

	// ---- 跨患者隔离：另一名患者只查得到自己的那 1 条生效事件 ----
	otherEvents := mustAudit(store, doctorA, other.ID, "查询另一名患者的审计")
	if len(otherEvents) != 1 || otherEvents[0].ObjectID != otherDraft.ID ||
		otherEvents[0].PatientID != other.ID {
		die("另一名患者应只有自己的 1 条生效事件，实际 %+v", otherEvents)
	}
	fmt.Printf("跨患者隔离: 另一名患者的审计只有 %d 条（其诊断生效，事件 %s）；"+
		"目标患者的 3 条事件一条都不过去，反之亦然\n",
		len(otherEvents), otherEvents[0].ID)

	// ---- 对照：正文与更正原因走已有的完整历史查询，不走审计 ----
	history := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	if history.CurrentVersion == nil || history.CurrentVersion.Number != 2 ||
		len(history.Versions) != 2 ||
		history.Versions[0].Content != v1.Content ||
		history.Versions[1].Content != v2.Content ||
		history.Versions[1].Reason != v2.Reason {
		die("完整历史查询应给出两版正文与更正原因，实际 %+v", history)
	}
	fmt.Printf("完整历史对照: EncounterRecords 给出第 1 版正文 %q、第 2 版正文 %q 与更正原因 %q；"+
		"这些内容审计事件一概不含——两类查询回答不同的问题\n",
		history.Versions[0].Content, history.Versions[1].Content, history.Versions[1].Reason)

	// ---- 失败一：接收方即使持有该患者诊断的有效授权，也不能调用内部查询 ----
	// 患者停用前已可为其授权（授权窗口覆盖固定时刻即可）；这里改在另一名
	// 仍活动的患者上演示，以保证授权本身有效——结论与患者状态无关。
	receiverGrant, err := store.Grant(doctorA, other.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: otherEnc.ID, Category: clinical.Diagnosis}},
		clock.Add(-24*time.Hour), clock.Add(24*time.Hour))
	must("为接收方建立另一名患者诊断的有效授权", err)
	// 接收方凭授权可以走自己的 Read 入口读到当前生效版本。
	rres, err := store.Read(receiver, other.ID, otherEnc.ID, clinical.Diagnosis)
	must("接收方凭授权读取另一名患者诊断", err)
	if len(rres.Records) != 1 || rres.Records[0].RecordID != otherDraft.ID {
		die("接收方应能读到被授权的 1 条诊断，实际 %+v", rres.Records)
	}
	// 但审计查询是内部入口：持权调用一律 ErrAccessDenied，且没有事件返回。
	denied, deniedErr := store.AuditEvents(receiver, other.ID)
	if !errors.Is(deniedErr, clinical.ErrAccessDenied) || denied != nil {
		die("接收方调用 AuditEvents 应返回 nil 与 ErrAccessDenied，实际得到 %d 条、%v",
			len(denied), deniedErr)
	}
	// 对已停用的目标患者同样被拒。
	deniedOff, deniedOffErr := store.AuditEvents(receiver, patient.ID)
	if !errors.Is(deniedOffErr, clinical.ErrAccessDenied) || deniedOff != nil {
		die("接收方查询已停用患者的审计应返回 nil 与 ErrAccessDenied，实际得到 %d 条、%v",
			len(deniedOff), deniedOffErr)
	}
	fmt.Printf("\n权限边界: 接收方持有有效授权（授权 %s）时 Read 能读到 %d 条当前生效诊断，"+
		"但调用 AuditEvents 被拒绝（%v）、返回事件数=%d；查询已停用的目标患者同样被拒绝、事件数=%d\n",
		receiverGrant.ID, len(rres.Records), clinical.ErrAccessDenied, len(denied), len(deniedOff))

	// ---- 失败二：内部使用者查询不存在的患者 ----
	missing, missingErr := store.AuditEvents(doctorA, "pat_does_not_exist")
	if !errors.Is(missingErr, clinical.ErrNotFound) || missing != nil {
		die("查询不存在患者应返回 nil 与 ErrNotFound，实际得到 %d 条、%v",
			len(missing), missingErr)
	}
	fmt.Printf("失败结果: 内部使用者查询不存在的患者得到 %v，返回事件数=%d（不带回任何事件）\n",
		clinical.ErrNotFound, len(missing))

	// ---- 成功的空清单：现有患者尚未产生任何审计 ----
	quiet, err := store.RegisterPatient(doctorA, "合成患者丑")
	must("登记尚无任何审计的合成患者", err)
	emptyEvents, err := store.AuditEvents(doctorB, quiet.ID)
	must("查询尚无审计的现有患者", err)
	fmt.Printf("成功的空清单: 现有患者 %s 尚未产生审计时返回错误=%v、事件数=%d"+
		"（空清单是成功结果，不表示患者不存在，也不表示没有权限）\n",
		quiet.ID, err, len(emptyEvents))
}

// mustAudit 由内部使用者查询指定患者的审计；查询失败立即终止，不把失败
// 返回值当作正式历史继续展示。
func mustAudit(store *clinical.Store, actor clinical.Actor, patientID clinical.ID, step string) []clinical.AuditEvent {
	events, err := store.AuditEvents(actor, patientID)
	must(step, err)
	return events
}

// mustHistory 由内部使用者查询指定就诊下的记录，并按记录标识取回这一条
// 记录的完整视图（含正文与更正原因），仅用于与审计事件做内容对照。
func mustHistory(store *clinical.Store, actor clinical.Actor, patientID, encounterID, recordID clinical.ID) clinical.RecordHistory {
	rows, err := store.EncounterRecords(actor, patientID, encounterID)
	must("内部使用者查询就诊完整历史", err)
	for i := range rows {
		if rows[i].Record.ID == recordID {
			return rows[i]
		}
	}
	die("完整历史中找不到记录 %q", recordID)
	return clinical.RecordHistory{}
}

// printAuditEvent 逐字段打印一条审计事件：事件标识、所属患者、操作人、
// 动作、对象类型、对象标识与发生时间。
func printAuditEvent(index int, ev clinical.AuditEvent) {
	fmt.Printf("  事件[%d]: 事件标识=%s，所属患者=%s，操作人=%s，动作=%s，"+
		"对象类型=%s，对象标识=%s，发生时间=%s\n",
		index, ev.ID, ev.PatientID, ev.ActorID, ev.Action,
		ev.ObjectType, ev.ObjectID, ev.OccurredAt.Format(time.RFC3339Nano))
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
