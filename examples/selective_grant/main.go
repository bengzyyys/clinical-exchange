// 命令 selective_grant 演示“只共享指定诊断”的限定记录授权：在同一次就诊
// 中准备两条已生效诊断与一条诊断草稿，用 GrantSelective 只选出第一条生效
// 诊断（整类范围留空），接收方 Read 时只能看到这一条记录的当前生效版本。
//
// 随后演示：
//
//  1. 更正被选中的诊断后，再次读取展示新的版本号与正文；旧版本与更正原因
//     仍不向接收方提供——选中的是记录而非某个固定版本。
//  2. 同一就诊同类后来新增并生效的记录不会自动进入这条限定授权。
//  3. 同一接收方另有一条覆盖该范围的整类授权时，读取合并全部有效授权
//     允许的记录，重叠记录只出现一次——限定授权不是对其他授权的缩减。
//  4. 把一条合法生效诊断与那条草稿一起放入限定选择：整条授权被拒绝
//     （ErrInvalidArgument），不保留合法部分，也不新增授权创建审计，
//     原先允许读取的内容保持原样。
//  5. 整类范围与限定选择同时为空同样被拒绝——空选择不表示共享全部诊断。
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
	dir, err := os.MkdirTemp("", "clinical-selective-grant-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备资料：合成患者、一次就诊、两条已生效诊断与一条诊断草稿 ----
	// 任一步失败都直接终止，不把失败返回值当作正式记录继续使用。
	patient, err := store.RegisterPatient(doctor, "合成患者丁")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	diag1, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断一：高血压 I10")
	must("创建第一条诊断草稿", err)
	diag1V1, err := store.ActivateRecord(doctor, diag1.ID)
	must("生效第一条诊断", err)

	diag2, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断二：2 型糖尿病 E11")
	must("创建第二条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag2.ID)
	must("生效第二条诊断", err)

	// 第三条只建草稿、不生效：它不应出现在任何接收方读取结果中。
	draft, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断草稿：甲状腺功能异常待查")
	must("创建诊断草稿（不生效）", err)

	// ---- 建立限定授权：整类范围留空，只明确选出第一条生效诊断 ----
	// 选择中显式填写记录标识及其所属就诊与类别；时间窗 [now, now+24h)
	// 覆盖随后的读取时刻。接收方此时只持有这一条限定授权。
	now := time.Now()
	grant, err := store.GrantSelective(doctor, patient.ID, receiver.ID,
		nil, // 整类范围留空：不按类别共享
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,
			Category:    clinical.Diagnosis,
			RecordID:    diag1.ID,
		}},
		now, now.Add(24*time.Hour))
	must("建立限定授权", err)
	fmt.Printf("限定授权已建立：整类范围 %d 项，限定选择 %d 条\n",
		len(grant.Scopes), len(grant.Selections))

	// ---- 接收方读取：只得到第一条诊断的当前生效内容 ----
	res, err := store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].RecordID != diag1.ID {
		die("限定授权下应只读到第一条诊断，实际读到 %d 条", len(res.Records))
	}
	fmt.Printf("首次读取：仅 1 条，正文=%q，第 %d 版\n",
		res.Records[0].Content, res.Records[0].Version)

	// ---- 更正被选中的诊断：读取展示新版本号与正文 ----
	// 选中的是记录而非某个固定版本：更正后授权覆盖它的当前版本。
	// EffectiveRecord 只有当前生效版本的标识、版本号、正文与生效时间，
	// 旧版本与更正原因（Version.Reason）不在读取结果中。
	diag1V2, err := store.CorrectRecord(doctor, diag1.ID, diag1V1.Number,
		"合成诊断一：高血压 I10（复核确认）", "补录复核依据")
	must("更正第一条诊断", err)
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("更正后接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].Version != diag1V2.Number ||
		res.Records[0].VersionID != diag1V2.ID {
		die("更正后应读到第一条诊断的第 %d 版，实际 %+v", diag1V2.Number, res.Records)
	}
	fmt.Printf("更正后读取：仍仅 1 条，正文=%q，第 %d 版（旧版本与更正原因不提供）\n",
		res.Records[0].Content, res.Records[0].Version)

	// ---- 后来新增并生效的同类记录不会自动进入这条限定授权 ----
	diag3, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断三：高脂血症 E78.5")
	must("创建第三条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag3.ID)
	must("生效第三条诊断", err)
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("新增诊断后接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].RecordID != diag1.ID {
		die("新增生效诊断不应进入限定授权，实际读到 %d 条", len(res.Records))
	}
	fmt.Printf("新增并生效第三条诊断后读取：仍仅 1 条（新记录不自动进入限定授权）\n")

	// ---- 失败演示一：合法生效诊断与草稿一起放入限定选择 ----
	// 任一选择不合法即拒绝整条授权：不保留合法部分，也不新增授权创建审计。
	auditsBefore, err := store.AuditEvents(doctor, patient.ID)
	must("查看授权前审计", err)
	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID, nil,
		[]clinical.RecordSelection{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: diag2.ID},
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: draft.ID},
		},
		now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("选择中含草稿应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("限定选择夹带草稿：整条授权被拒绝（%v）\n", clinical.ErrInvalidArgument)

	auditsAfter, err := store.AuditEvents(doctor, patient.ID)
	must("查看授权后审计", err)
	if len(auditsAfter) != len(auditsBefore) {
		die("失败的授权不应新增审计：之前 %d 条，之后 %d 条", len(auditsBefore), len(auditsAfter))
	}
	fmt.Printf("失败授权未保留合法部分、未新增授权创建审计：审计条数不变=%v\n",
		len(auditsAfter) == len(auditsBefore))

	// 原先允许读取的内容保持原样：仍只有第一条诊断的当前版本。
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("失败授权后接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].RecordID != diag1.ID ||
		res.Records[0].Version != diag1V2.Number {
		die("失败授权后读取应保持原样，实际读到 %d 条", len(res.Records))
	}
	fmt.Printf("失败授权后读取：仍仅第一条诊断第 %d 版，原授权不受影响\n", diag1V2.Number)

	// ---- 失败演示二：整类范围与限定选择同时为空 ----
	// 空选择不表示共享全部诊断，同样返回 ErrInvalidArgument。
	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID,
		nil, nil, now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("整类范围与限定选择同时为空应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("整类范围与限定选择同时为空：被拒绝（%v），空选择不表示共享全部诊断\n",
		clinical.ErrInvalidArgument)

	// ---- 合并演示：同一接收方另有一条覆盖该范围的整类授权 ----
	// 读取合并全部有效授权允许的记录，重叠记录只出现一次；限定授权
	// 不是对其他授权的缩减。
	_, err = store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must("建立整类授权", err)
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("整类授权后接收方读取诊断", err)
	seen := map[clinical.ID]int{}
	for _, r := range res.Records {
		seen[r.RecordID]++
	}
	mergeOK := len(res.Records) == 3 &&
		seen[diag1.ID] == 1 && seen[diag2.ID] == 1 && seen[diag3.ID] == 1 &&
		seen[draft.ID] == 0
	if !mergeOK {
		die("整类+限定合并后应读到 3 条生效诊断且各一次，实际 %+v", res.Records)
	}
	fmt.Printf("另有整类授权后读取：合并为 %d 条生效诊断，重叠的第一条只出现 %d 次，草稿仍不出现\n",
		len(res.Records), seen[diag1.ID])
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
