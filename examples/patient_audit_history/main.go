// 命令 patient_audit_history 演示内部使用者如何用 AuditEvents 按患者查看
// 审计历史：不必先取得包含病历正文的完整档案，就能核对“谁在什么时候操作了
// 哪条记录”。
//
// 示例围绕一名合成患者（合成患者癸）的一条诊断展开，全程只使用合成资料：
//   - 内部使用者 doctor-1 把诊断草稿生效，doctor-2 随后更正同一条诊断；
//   - 期间穿插另一名合成患者（合成患者子）的一次生效操作，用来证明按患者
//     查询只返回目标患者自己的事件；
//   - 随后停用目标患者；停用后查询，结果仍按真实操作先后包含生效、更正与
//     最后的停用事件，且查询本身不新增审计事件；
//   - 生效与更正事件指向同一条记录（对象类型 record、对象标识为记录标识），
//     事件自己的标识（aud_ 开头）与记录标识（rec_ 开头）分别打印、不能混淆；
//     停用事件指向患者档案（对象类型 patient、对象标识为患者标识）；
//   - 审计事件不含病历正文与更正原因：需要这些内容时仍使用已有的完整历史
//     查询（EncounterRecords / Chart），一条更正事件不能代替完整更正记录；
//   - 两名内部使用者的两次操作刻意落在同一时刻，结果仍保留真实发生顺序；
//   - 权限与失败：持有该患者诊断有效授权的接收方调用得到 ErrAccessDenied 且
//     没有事件；内部使用者查询不存在的患者得到 ErrNotFound 且没有事件；
//     现有但尚未产生审计的患者成功返回空清单。
//
// 运行：
//
//	go run ./examples/patient_audit_history
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

	// 注入固定时钟：让 doctor-1 生效与 doctor-2 更正落在同一时刻，演示
	// “两次操作即使时间相同，也保留真实发生顺序”。
	base := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	clock := base
	store, err := clinical.Open(dir, clinical.WithClock(func() time.Time { return clock }))
	must("打开本地存储", err)
	defer store.Close()

	doctor1 := clinical.InternalActor("doctor-1")
	doctor2 := clinical.InternalActor("doctor-2")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备目标患者与一条诊断草稿 ----
	patient, err := store.RegisterPatient(doctor1, "合成患者癸")
	must("登记目标合成患者", err)
	encounter, err := store.AddEncounter(doctor1, patient.ID, base.Add(-24*time.Hour))
	must("登记就诊", err)
	diag, err := store.CreateDraft(doctor1, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断：急性支气管炎 J20")
	must("创建诊断草稿", err)

	// ---- 第一次成功操作：doctor-1 使草稿生效（时钟停在 base） ----
	v1, err := store.ActivateRecord(doctor1, diag.ID)
	must("doctor-1 生效诊断草稿", err)

	// ---- 第二次成功操作：doctor-2 更正同一条诊断 ----
	// 刻意不推进时钟：与生效同一时刻发生，仍必须排在生效之后。
	// 更正原因随版本保存在完整历史里，审计事件本身不携带它。
	v2, err := store.CorrectRecord(doctor2, diag.ID, v1.Number,
		"合成诊断：急性支气管炎 J20（胸片已复核）", "复核胸片后补充检查结论")
	must("doctor-2 更正诊断（与生效同一时刻）", err)

	// ---- 穿插：另一名合成患者的一次生效操作，不应混入目标患者的审计 ----
	other, err := store.RegisterPatient(doctor1, "合成患者子")
	must("登记另一名合成患者", err)
	otherEncounter, err := store.AddEncounter(doctor1, other.ID, base.Add(-24*time.Hour))
	must("登记另一名患者的就诊", err)
	otherDraft, err := store.CreateDraft(doctor1, other.ID, otherEncounter.ID,
		clinical.Diagnosis, "合成诊断：另一名患者的普通感冒 J00")
	must("创建另一名患者的诊断草稿", err)
	if _, err := store.ActivateRecord(doctor1, otherDraft.ID); err != nil {
		die("另一名患者的诊断生效失败: %v", err)
	}

	// ---- 为接收方建立覆盖目标患者诊断的有效授权（授权自身会留一条审计） ----
	// 授权是为了证明“接收方即使持有该患者诊断的有效授权，也不能调用这个
	// 内部查询”；它本身的创建事件属于目标患者，会出现在审计历史中。
	grant, err := store.Grant(doctor1, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		base.Add(-time.Hour), base.Add(24*time.Hour))
	must("为接收方建立目标患者诊断的有效授权", err)

	// ---- 第三次成功操作：停用目标患者（推进时钟，时间与前两次不同） ----
	clock = base.Add(time.Hour)
	must("停用目标患者", store.DeactivatePatient(doctor1, patient.ID))

	fmt.Println("准备完成: 目标患者的诊断经 doctor-1 生效、doctor-2 更正（两次同一时刻）；")
	fmt.Printf("  期间另一名患者 %s 完成一次生效；随后停用目标患者。诊断记录标识=%s\n", other.ID, diag.ID)
	fmt.Printf("  生效版本=%s（版本 %d），更正版本=%s（版本 %d，原因只在完整历史中）；授权=%s\n",
		v1.ID, v1.Number, v2.ID, v2.Number, grant.ID)

	// ---- 停用后查询目标患者的审计 ----
	events, err := store.AuditEvents(doctor1, patient.ID)
	must("停用后查询目标患者审计", err)
	fmt.Printf("\n停用后 AuditEvents: 错误=%v，事件数=%d，全部属于目标患者=%v，按实际操作先后返回=%v\n",
		err, len(events), allBelongTo(events, patient.ID), inOccurrenceOrder(events))

	want := []struct {
		actor      string
		action     string
		objectType string
		objectID   clinical.ID
		at         time.Time
	}{
		{doctor1.ID, clinical.ActionActivated, "record", diag.ID, base},
		{doctor2.ID, clinical.ActionCorrected, "record", diag.ID, base},
		{doctor1.ID, clinical.ActionGranted, "authorization", grant.ID, base},
		{doctor1.ID, clinical.ActionDeactivated, "patient", patient.ID, base.Add(time.Hour)},
	}
	if len(events) != len(want) {
		die("目标患者应有 %d 条审计（生效、更正、授权创建、停用），实际 %d 条: %s",
			len(want), len(events), eventIDList(events))
	}
	for i, w := range want {
		ev := events[i]
		if ev.ActorID != w.actor || ev.Action != w.action ||
			ev.ObjectType != w.objectType || ev.ObjectID != w.objectID ||
			ev.PatientID != patient.ID || !ev.OccurredAt.Equal(w.at) {
			die("事件 %d 与实际操作不符:\n got=%+v\nwant 操作人=%s 动作=%s 对象=%s/%s 时间=%v",
				i, ev, w.actor, w.action, w.objectType, w.objectID, w.at)
		}
		if ev.ID == "" {
			die("事件 %d 缺少自己的事件标识", i)
		}
	}
	for i := range events {
		printEvent(i, events[i])
	}

	// ---- 核对点一：生效与更正指向同一条记录；事件标识不是记录标识 ----
	actEvent, corrEvent := events[0], events[1]
	if actEvent.ObjectType != "record" || corrEvent.ObjectType != "record" ||
		actEvent.ObjectID != diag.ID || corrEvent.ObjectID != diag.ID {
		die("生效与更正事件都应指向同一条记录 %s，实际 %s/%s 与 %s/%s",
			diag.ID, actEvent.ObjectType, actEvent.ObjectID, corrEvent.ObjectType, corrEvent.ObjectID)
	}
	if actEvent.ID == diag.ID || corrEvent.ID == diag.ID || actEvent.ID == corrEvent.ID {
		die("事件标识与记录标识必须各自独立、互不相同: 生效事件=%s 更正事件=%s 记录=%s",
			actEvent.ID, corrEvent.ID, diag.ID)
	}
	fmt.Printf("\n指向关系: 生效事件 %s 与更正事件 %s 都指向记录 %s（同一对象标识）；"+
		"事件标识彼此不同、也不同于记录标识\n", actEvent.ID, corrEvent.ID, diag.ID)

	// ---- 核对点二：两次操作时间相同，仍按真实先后返回 ----
	if !actEvent.OccurredAt.Equal(corrEvent.OccurredAt) {
		die("本示例的生效与更正刻意同一时刻，实际 %v 与 %v",
			actEvent.OccurredAt, corrEvent.OccurredAt)
	}
	fmt.Printf("同刻保序: 生效与更正发生时间都是 %v，生效仍排在更正之前；"+
		"事件标识只用于区分事件，不表示操作先后\n", actEvent.OccurredAt.Format(time.RFC3339))

	// ---- 核对点三：停用事件指向患者档案，且是最后一条 ----
	stopEvent := events[len(events)-1]
	if stopEvent.Action != clinical.ActionDeactivated ||
		stopEvent.ObjectType != "patient" || stopEvent.ObjectID != patient.ID {
		die("最后一条应是指向患者档案的停用事件，实际 %+v", stopEvent)
	}
	fmt.Printf("停用事件: %s 指向患者档案（对象类型=%s，对象标识=%s），排在历史末尾，发生时间=%s\n",
		stopEvent.ID, stopEvent.ObjectType, stopEvent.ObjectID, stopEvent.OccurredAt.Format(time.RFC3339))

	// ---- 核对点四：另一名患者的生效不混入；目标患者的事件也不出现在其历史中 ----
	otherEvents, err := store.AuditEvents(doctor1, other.ID)
	must("查询另一名患者的审计", err)
	if len(otherEvents) != 1 || otherEvents[0].Action != clinical.ActionActivated ||
		otherEvents[0].ObjectID != otherDraft.ID || otherEvents[0].PatientID != other.ID {
		die("另一名患者应只有自己的 1 条生效事件，实际 %+v", otherEvents)
	}
	for _, ev := range events {
		if ev.PatientID != patient.ID {
			die("目标患者结果混入了其他患者的事件: %+v", ev)
		}
	}
	fmt.Printf("跨患者隔离: 另一名患者的历史只有自己的生效事件（事件 %s 指向记录 %s）=%v；"+
		"目标患者的 %d 条事件全部归属自己=%v\n",
		otherEvents[0].ID, otherDraft.ID,
		otherEvents[0].ObjectID == otherDraft.ID && otherEvents[0].PatientID == other.ID,
		len(events), allBelongTo(events, patient.ID))

	// ---- 核对点五：查询是只读的，再查一次得到同一份历史，不新增事件 ----
	again, err := store.AuditEvents(doctor2, patient.ID)
	must("由另一名内部使用者再次查询", err)
	if len(again) != len(events) || !sameEvents(again, events) {
		die("审计查询不应改变历史：首次 %s，再次 %s", eventIDList(events), eventIDList(again))
	}
	fmt.Printf("只读核对: 换另一名内部使用者再查，仍是同样 %d 条、标识与次序不变=%v——"+
		"内部使用者查到的是该患者的全部审计，不只自己做过的操作；查询本身不新增事件\n",
		len(again), sameEvents(again, events))

	// ---- 审计里没有正文与更正原因：需要这些内容仍走已有的完整历史查询 ----
	// 更正事件只能告诉我们“doctor-2 在何时更正了哪条记录”；更正后的正文与
	// 更正原因属于版本历史，要用 EncounterRecords（或完整档案 Chart）取得。
	histories, err := store.EncounterRecords(doctor1, patient.ID, encounter.ID)
	must("查询完整记录历史", err)
	history := findRecord(histories, diag.ID)
	if history == nil || history.CurrentVersion == nil {
		die("完整历史中应能找到该诊断及其当前版本，实际 %s", recordIDList(histories))
	}
	if history.CurrentVersion.ID != v2.ID || history.CurrentVersion.Content != v2.Content ||
		history.CurrentVersion.Reason == "" {
		die("完整历史应给出更正后的正文与非空更正原因，实际当前版本=%+v", history.CurrentVersion)
	}
	fmt.Printf("\n正文与更正原因不在审计中: 更正事件只含操作人/动作/对象/时间；需要“改成了什么、为什么改”\n")
	fmt.Printf("  仍用完整历史查询 EncounterRecords/Chart：当前版本 %s，正文=%q，更正原因=%q\n",
		history.CurrentVersion.ID, history.CurrentVersion.Content, history.CurrentVersion.Reason)
	fmt.Printf("  一条更正审计事件不能代替这条完整更正记录（共 %d 版，旧版正文也在完整历史中）\n",
		len(history.Versions))

	// ---- 失败一：接收方持有有效授权，也不能调用这个内部查询 ----
	denied, deniedErr := store.AuditEvents(receiver, patient.ID)
	if !errors.Is(deniedErr, clinical.ErrAccessDenied) || denied != nil {
		die("接收方查询审计应返回 nil 与 ErrAccessDenied，实际得到 %d 条、%v",
			len(denied), deniedErr)
	}
	fmt.Printf("\n失败一（接收方持有效授权调用内部查询）: %v，返回事件数=%d（不泄露任何审计事件）\n",
		deniedErr, len(denied))

	// ---- 失败二：内部使用者查询不存在的患者 ----
	missing, missingErr := store.AuditEvents(doctor1, "pat_does_not_exist")
	if !errors.Is(missingErr, clinical.ErrNotFound) || missing != nil {
		die("查询不存在患者应返回 nil 与 ErrNotFound，实际得到 %d 条、%v",
			len(missing), missingErr)
	}
	fmt.Printf("失败二（内部使用者查询不存在的患者）: %v，返回事件数=%d（ErrNotFound）\n",
		missingErr, len(missing))

	// ---- 成功的空清单：现有患者尚未产生任何审计 ----
	fresh, err := store.RegisterPatient(doctor1, "合成患者丑")
	must("登记一名尚无任何审计的合成患者", err)
	empty, err := store.AuditEvents(doctor1, fresh.ID)
	must("查询尚无审计的现有患者", err)
	if len(empty) != 0 {
		die("尚无审计的现有患者应返回空清单，实际 %d 条: %s", len(empty), eventIDList(empty))
	}
	fmt.Printf("成功的空清单: 现有患者 %s 尚未产生审计，成功返回 %d 条（错误=%v）"+
		"——不是患者不存在，也不是没有查询权限\n", fresh.ID, len(empty), err)
}

// printEvent 逐条打印事件自己的标识、所属患者、操作人、动作、对象类型、
// 对象标识与发生时间，便于把每条事件对应到一次成功操作。
func printEvent(index int, ev clinical.AuditEvent) {
	fmt.Printf("  事件[%d]: 事件标识=%s，所属患者=%s，操作人=%s，动作=%s，对象类型=%s，对象标识=%s，发生时间=%s\n",
		index, ev.ID, ev.PatientID, ev.ActorID, ev.Action,
		ev.ObjectType, ev.ObjectID, ev.OccurredAt.Format(time.RFC3339))
}

// allBelongTo 核对返回事件的所属患者全部一致。
func allBelongTo(events []clinical.AuditEvent, patientID clinical.ID) bool {
	for _, ev := range events {
		if ev.PatientID != patientID {
			return false
		}
	}
	return true
}

// inOccurrenceOrder 核对事件按真实发生顺序返回：时间不回退；时间相同的相邻
// 事件也允许（顺序由追加次序保留，不能在这里按时间重排）。
func inOccurrenceOrder(events []clinical.AuditEvent) bool {
	for i := 1; i < len(events); i++ {
		if events[i].OccurredAt.Before(events[i-1].OccurredAt) {
			return false
		}
	}
	return true
}

// sameEvents 核对两次查询得到的事件标识、次序与内容完全一致。
func sameEvents(a, b []clinical.AuditEvent) bool {
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

func findRecord(hs []clinical.RecordHistory, id clinical.ID) *clinical.RecordHistory {
	for i := range hs {
		if hs[i].Record.ID == id {
			return &hs[i]
		}
	}
	return nil
}

func recordIDList(hs []clinical.RecordHistory) []clinical.ID {
	ids := make([]clinical.ID, len(hs))
	for i, h := range hs {
		ids[i] = h.Record.ID
	}
	return ids
}

func eventIDList(events []clinical.AuditEvent) []clinical.ID {
	ids := make([]clinical.ID, len(events))
	for i, ev := range events {
		ids[i] = ev.ID
	}
	return ids
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
