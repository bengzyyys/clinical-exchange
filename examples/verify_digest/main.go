// 命令 verify_digest 面向接收方演示“取包后如何独立核对摘要”。
//
// 接收方取包（FetchPackage）拿到的是固化的包内容（clinical.Package）与服务
// 附在交付上的摘要（PackageDelivery.Digest）。本示例不依赖任何未导出能力：
// 接收方按文档中公开的规范序列化规则，在本地从“收到的字段”重算一遍摘要，
// 再与服务摘要逐字比较——
//
//	本地摘要 == 服务摘要：收到的内容与服务固化并标注的那份逐字节一致；
//	本地摘要 != 服务摘要：收到的内容至少有一处与服务保存的不同（哪怕只改
//	                     一个字符、一个换行、丢一位小数秒）。
//
// 示例分两部分：
//
//	第一部分用文档中的固定金向量（确定的输入、确定的期望摘要），证明本地
//	重算规则可复现既有交付；只改正文一个字符即得到不同摘要。
//	第二部分走真实公开流程（登记合成患者→就诊→诊断/医嘱→授权→创建交换→
//	接收方取包），对真实交付做本地核对、重排/时区等价核对、正文篡改核对，
//	最后说明 SubmitReceipt 只比较“提交的摘要字符串”与“交换保存的摘要”，
//	并不重新检查接收方手中的临床内容。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/verify_digest
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	// ===== 第一部分：固定金向量，输入与期望摘要都是确定的 =====

	golden := clinical.Package{
		PatientID:  "pat_digest_golden_01",
		ReceiverID: "rcv_digest_golden",
		Records: []clinical.PackagedRecord{
			// 故意把医嘱放在诊断前面：规范序列化会先按 record_id 排序，
			// 入参排列不影响摘要。
			{
				EncounterID: "enc_digest_golden_01",
				Category:    clinical.Order,
				RecordID:    "rec_ord_golden",
				VersionID:   "ver_ord_golden_7th",
				Version:     7,
				EffectiveAt: time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC),
				Content:     "医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注",
			},
			{
				EncounterID: "enc_digest_golden_01",
				Category:    clinical.Diagnosis,
				RecordID:    "rec_diag_golden",
				VersionID:   "ver_diag_golden_2nd",
				Version:     2,
				EffectiveAt: time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC),
				Content:     "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历",
			},
		},
	}

	const wantGoldenDigest = "f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87"

	fmt.Println("== 金向量：按文档规则在本地从收到的字段重算 ==")
	fmt.Printf("规范字节: %s\n", mustCanonical(golden))
	goldenLocal := ReceiverDigest(golden)
	fmt.Printf("本地摘要: %s\n", goldenLocal)
	fmt.Printf("核对结果: 本地摘要 == 期望摘要 %v\n", goldenLocal == wantGoldenDigest)

	// 只改收到的诊断正文中的一个字符（小于号 -> 书名号），其余全部不动。
	tampered := golden
	tampered.Records = append([]clinical.PackagedRecord(nil), golden.Records...)
	tampered.Records[1].Content = "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a《b 与 c&d；目录 C:\\病历"
	tamperedLocal := ReceiverDigest(tampered)
	fmt.Printf("只改一个字符后的本地摘要: %s\n", tamperedLocal)
	fmt.Printf("核对结果: 改动后摘要 == 原期望摘要 %v；原先保存的服务摘要仍是 %s\n",
		tamperedLocal == wantGoldenDigest, wantGoldenDigest)

	// ===== 第二部分：真实公开流程，对一份合成交换包端到端核对 =====

	fmt.Println()
	fmt.Println("== 端到端：真实合成交换包的取包核对与回执 ==")

	dir, err := os.MkdirTemp("", "clinical-verify-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	// 固定时钟到带小数秒的时刻，使生效时间保留纳秒小数（非整秒）。
	clockTime := time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC)
	store, err := clinical.Open(dir, clinical.WithClock(func() time.Time { return clockTime }))
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	patient, err := store.RegisterPatient(doctor, "摘要核对合成患者")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, clockTime.Add(-3*time.Hour))
	must("登记就诊", err)

	// 诊断与医嘱正文都含中文、换行、半角双引号、反斜杠、小于号、与号。
	diagDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Diagnosis,
		"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历")
	must("创建诊断草稿", err)
	if _, err := store.ActivateRecord(doctor, diagDraft.ID); err != nil {
		die("生效诊断失败: %v", err)
	}
	orderDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Order,
		"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注")
	must("创建医嘱草稿", err)
	if _, err := store.ActivateRecord(doctor, orderDraft.ID); err != nil {
		die("生效医嘱失败: %v", err)
	}

	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis},
			{EncounterID: encounter.ID, Category: clinical.Order},
		},
		clockTime.Add(-24*time.Hour), clockTime.Add(24*time.Hour))
	must("建立授权", err)

	// 故意“医嘱在前、诊断在后”提交：服务固化时按 record_id 排序，与入参顺序无关。
	exchange, err := store.CreateExchange(doctor, patient.ID, receiver.ID, grant.ID,
		[]clinical.ID{orderDraft.ID, diagDraft.ID}, "request-verify-digest")
	must("创建交换", err)

	delivery, err := store.FetchPackage(receiver, exchange.ID)
	must("接收方取包", err)

	serviceDigest := delivery.Digest //取包时服务交付并保存的摘要：核对的比较基准，应原样留存。

	// 1) 正常输入：本地按收到的字段重算，与服务摘要一致。
	localDigest := ReceiverDigest(delivery.Package)
	fmt.Printf("正常取包核对: 本地摘要 == 服务摘要 %v（服务摘要=%s）\n",
		localDigest == serviceDigest, serviceDigest)
	fmt.Println("说明: 端到端各标识由服务随机生成，故该十六进制值每次运行不同；")
	fmt.Println("      确定性的固定值见上方金向量。此处要对照的是各“是否一致”的结论。")

	// 2) 仅改变记录排列顺序：内容未变，摘要必须相同。
	reordered := delivery.Package
	reordered.Records = append([]clinical.PackagedRecord(nil), delivery.Package.Records...)
	reordered.Records[0], reordered.Records[1] = reordered.Records[1], reordered.Records[0]
	fmt.Printf("仅调换记录顺序: 本地摘要 == 服务摘要 %v\n",
		ReceiverDigest(reordered) == serviceDigest)

	// 3) 同一时刻换时区表示（+9/-5 与 UTC 是同一瞬间）：内容未变，摘要必须相同。
	east := time.FixedZone("UTC+9", 9*60*60)
	west := time.FixedZone("UTC-5", -5*60*60)
	rezoned := delivery.Package
	rezoned.Records = append([]clinical.PackagedRecord(nil), delivery.Package.Records...)
	rezoned.Records[0].EffectiveAt = rezoned.Records[0].EffectiveAt.In(east)
	rezoned.Records[1].EffectiveAt = rezoned.Records[1].EffectiveAt.In(west)
	fmt.Printf("仅换同一时刻的时区表示: 本地摘要 == 服务摘要 %v\n",
		ReceiverDigest(rezoned) == serviceDigest)

	// 4) 只改收到的诊断正文一个字符：本地摘要立即不同；保存的服务摘要保持原值，
	//    仍是比较依据。
	received := delivery.Package
	received.Records = append([]clinical.PackagedRecord(nil), delivery.Package.Records...)
	for i := range received.Records {
		if received.Records[i].Category == clinical.Diagnosis {
			received.Records[i].Content =
				"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a《b 与 c&d；目录 C:\\病历"
		}
	}
	tamperedDigest := ReceiverDigest(received)
	fmt.Printf("只改收到的正文一个字符: 本地摘要 == 服务摘要 %v；本地新摘要 != 服务摘要 %v；服务摘要保持原值 %v\n",
		tamperedDigest == serviceDigest, tamperedDigest != serviceDigest, serviceDigest == delivery.Digest)

	// 5) 核对与回执的关系：SubmitReceipt 比较“提交的摘要”与“交换保存的摘要”，
	//    不重新检查接收方手中的临床内容。
	conf, err := store.SubmitReceipt(receiver, exchange.ID, serviceDigest, clinical.ReceiptAccepted, "")
	must("凭原服务摘要登记接受回执", err)
	fmt.Printf("凭原服务摘要登记回执: 成功，交换状态=%s，回执结果=%s\n", conf.Status, conf.Outcome)

	if _, err := store.SubmitReceipt(receiver, exchange.ID, tamperedDigest,
		clinical.ReceiptAccepted, ""); !errors.Is(err, clinical.ErrConflict) {
		die("用被改正文算出的摘要登记回执应得到 ErrConflict，实际 %v", err)
	}
	fmt.Printf("凭被改正文算出的摘要登记回执: 被拒绝（%v）；服务并未读取本地正文，只比对摘要字符串\n",
		clinical.ErrConflict)
}

// ReceiverDigest 是接收方一侧的独立重算：严格按文档的规范序列化规则，
// 从“收到的包字段”算出 64 位小写十六进制 sha256。它不调用交换库的任何
// 内部函数——这正是接收方不预先信任服务摘要、自行核对所需要做的事。
func ReceiverDigest(p clinical.Package) string {
	recs := append([]clinical.PackagedRecord(nil), p.Records...)
	// 记录先按 record_id 字节序升序排列，与入参/交付中的排列无关。
	sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })

	cp := canonicalPackage{
		PatientID:  p.PatientID,
		ReceiverID: p.ReceiverID,
		Records:    make([]canonicalRecord, 0, len(recs)),
	}
	for _, r := range recs {
		cp.Records = append(cp.Records, canonicalRecord{
			EncounterID: r.EncounterID,
			Category:    r.Category,
			RecordID:    r.RecordID,
			VersionID:   r.VersionID,
			Version:     r.Version,
			// 时间先转 UTC，再按 RFC3339Nano 表示（保留小数秒、去掉尾随零）。
			EffectiveAt: r.EffectiveAt.UTC().Format(time.RFC3339Nano),
			Content:     r.Content,
		})
	}
	// json.Marshal 默认紧凑无空白，并对 <、>、& 做 \u003c/\u003e/\u0026
	// 转义；中文等非 ASCII 字符按 UTF-8 原样写出。
	raw, err := json.Marshal(cp)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw) // 对精确的 UTF-8 字节序列求摘要，不追加换行。
	return hex.EncodeToString(sum[:])
}

func mustCanonical(p clinical.Package) string {
	recs := append([]clinical.PackagedRecord(nil), p.Records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })
	cp := canonicalPackage{
		PatientID:  p.PatientID,
		ReceiverID: p.ReceiverID,
		Records:    make([]canonicalRecord, 0, len(recs)),
	}
	for _, r := range recs {
		cp.Records = append(cp.Records, canonicalRecord{
			EncounterID: r.EncounterID,
			Category:    r.Category,
			RecordID:    r.RecordID,
			VersionID:   r.VersionID,
			Version:     r.Version,
			EffectiveAt: r.EffectiveAt.UTC().Format(time.RFC3339Nano),
			Content:     r.Content,
		})
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// 字段名与顺序固定的规范结构，与交换库内部摘要规则一致。
type canonicalRecord struct {
	EncounterID string `json:"encounter_id"`
	Category    string `json:"category"`
	RecordID    string `json:"record_id"`
	VersionID   string `json:"version_id"`
	Version     int    `json:"version"`
	EffectiveAt string `json:"effective_at"`
	Content     string `json:"content"`
}

type canonicalPackage struct {
	PatientID  string            `json:"patient_id"`
	ReceiverID string            `json:"receiver_id"`
	Records    []canonicalRecord `json:"records"`
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
