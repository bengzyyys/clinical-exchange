// 命令 selective_grant 演示“只共享指定诊断”的限定记录授权：同一次就诊中
// 准备两条已生效诊断和一条诊断草稿，内部使用者用 GrantSelective 只把第一条
// 已生效诊断授权给接收方（整类范围留空）。接收方读取该次就诊的诊断时只能
// 看到这一条；更正被选中诊断后读到新版本号与新正文，旧版本与更正原因不向
// 接收方提供；授权后新增并生效的同类记录不自动进入这条授权。随后演示两种
// 建立授权的失败：限定选择夹带草稿、整类范围与限定选择同时为空，二者都被
// 整体拒绝，不保留合法部分，也不新增授权创建审计。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/selective_grant
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-selective-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")
	stranger := clinical.ReceiverActor("insurer-2")

	// ---- 准备资料：合成患者、一次就诊、两条已生效诊断与一条诊断草稿 ----
	patient, err := store.RegisterPatient(doctor, "合成患者丁")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	diag1, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断一：高血压 I10")
	must("创建第一条诊断草稿", err)
	v1, err := store.ActivateRecord(doctor, diag1.ID)
	must("生效第一条诊断", err)

	diag2, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断二：2 型糖尿病 E11")
	must("创建第二条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag2.ID)
	must("生效第二条诊断", err)

	draft, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断草稿：内容待补充")
	must("创建诊断草稿（保持草稿状态）", err)
	fmt.Println("准备完成: 同一次就诊有两条已生效诊断和一条诊断草稿")

	// ---- 建立限定授权：整类范围留空，只选第一条已生效诊断 ----
	// 选择中明确填写记录标识及其所属就诊和类别；时间窗 [now, now+24h)
	// 覆盖下面的读取时刻。
	now := time.Now()
	grant, err := store.GrantSelective(doctor, patient.ID, receiver.ID,
		nil, // 整类范围留空：不共享该就诊下的全部诊断
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,       // 记录所属就诊
			Category:    clinical.Diagnosis, // 记录类别
			RecordID:    diag1.ID,           // 选中的记录标识
		}},
		now, now.Add(24*time.Hour))
	must("建立限定授权", err)
	fmt.Printf("建立限定授权: 成功，整类范围 %d 项，限定选择 %d 条\n",
		len(grant.Scopes), len(grant.Selections))

	// ---- 接收方读取：只得到第一条诊断的当前生效内容 ----
	res, err := store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("接收方首次读取", err)
	onlyFirst := len(res.Records) == 1 &&
		res.Records[0].RecordID == diag1.ID &&
		res.Records[0].Version == v1.Number &&
		res.Records[0].Content == "合成诊断一：高血压 I10"
	fmt.Printf("接收方首次读取: 记录数=%d，只含被选中的第一条（版本=%d）=%v，第二条与草稿均不出现\n",
		len(res.Records), res.Records[0].Version, onlyFirst)

	// 没有任何授权的其他接收方读取同一范围：ErrAccessDenied，结果为空。
	if _, err := store.Read(stranger, patient.ID, encounter.ID, clinical.Diagnosis); !errors.Is(err, clinical.ErrAccessDenied) {
		die("无授权接收方读取应返回 ErrAccessDenied，实际得到 %v", err)
	}
	fmt.Printf("无授权接收方读取: 被拒绝（%v），不泄露任何记录\n", clinical.ErrAccessDenied)

	// ---- 更正被选中的诊断：授权选中的是记录，读取跟随到它的当前版本 ----
	v2, err := store.CorrectRecord(doctor, diag1.ID, v1.Number,
		"合成诊断一：高血压 I10（复核确认）", "补录复核依据")
	must("更正被选中的诊断", err)

	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("更正后读取", err)
	updated := len(res.Records) == 1 &&
		res.Records[0].RecordID == diag1.ID &&
		res.Records[0].Version == v2.Number &&
		res.Records[0].Content == "合成诊断一：高血压 I10（复核确认）"
	fmt.Printf("更正后读取: 仍为 1 条，展示新版本号=%d 与新正文=%v；旧版本与更正原因不在读取结果中\n",
		res.Records[0].Version, updated)

	// ---- 授权后新增并生效的同类记录不自动进入这条限定授权 ----
	diag3, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断三：高脂血症 E78.5")
	must("创建第三条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag3.ID)
	must("生效第三条诊断", err)

	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("新增诊断后读取", err)
	stillOnlyFirst := len(res.Records) == 1 && res.Records[0].RecordID == diag1.ID
	fmt.Printf("授权后新增并生效一条诊断: 读取记录数=%d，仍只有第一条=%v（新记录不自动进入限定授权）\n",
		len(res.Records), stillOnlyFirst)

	// ---- 失败演示一：限定选择夹带草稿，整条授权不成立 ----
	auditsBefore, err := store.AuditEvents(doctor, patient.ID)
	must("查看审计", err)

	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID, nil,
		[]clinical.RecordSelection{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: diag2.ID}, // 合法生效诊断
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: draft.ID}, // 草稿：不合法
		}, now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("限定选择夹带草稿应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	// 授权未建立：不继续使用任何返回值，合法部分（第二条诊断）也不会被保留。
	fmt.Printf("限定选择夹带草稿: 被拒绝（%v），整条授权不成立，合法部分不保留\n", clinical.ErrInvalidArgument)

	// ---- 失败演示二：整类范围与限定选择同时为空 ----
	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID, nil, nil,
		now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("空范围空选择应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("整类范围与限定选择同时为空: 被拒绝（%v），空选择不表示共享全部诊断\n", clinical.ErrInvalidArgument)

	// 两次失败都不新增授权创建审计；原先允许读取的内容保持原样。
	auditsAfter, err := store.AuditEvents(doctor, patient.ID)
	must("再次查看审计", err)
	fmt.Printf("两次失败均未新增授权创建审计: %v\n",
		countGrants(auditsAfter) == countGrants(auditsBefore))

	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("失败后读取", err)
	unchanged := len(res.Records) == 1 &&
		res.Records[0].RecordID == diag1.ID &&
		res.Records[0].Version == v2.Number
	fmt.Printf("失败后读取: 内容保持原样（仍只有第一条的第 %d 版）=%v\n",
		res.Records[0].Version, unchanged)
}

// countGrants 统计审计事件中的授权创建条数。
func countGrants(events []clinical.AuditEvent) int {
	n := 0
	for _, e := range events {
		if e.Action == clinical.ActionGranted {
			n++
		}
	}
	return n
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
