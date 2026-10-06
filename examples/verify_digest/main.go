// 命令 verify_digest 演示接收方取包后如何核对“收到的内容”与“服务摘要”
// 是否一致。
//
// 现有交换功能只要求取包后保留摘要、在回执中原样回传，却没有交代接收方
// 怎样根据收到的包确认内容与摘要对得上。本示例完全站在接收方一侧：只用
// 取包得到的 PackageDelivery（交换标识、服务摘要、包内容），按与服务端
// 相同的公开规则自行重算摘要并逐字比较——不需要任何内部身份，也不需要
// 患者姓名、草稿、旧版本或更正原因（它们根本不在包内）。
//
// 演示分两部分：
//
//  1. 完整调用链（登记→生效→授权→创建交换→取包→本地核对→回执）：
//     正常收到的包核对一致；只改动收到的正文一个字后核对不一致，原先
//     保存的服务摘要仍在、仍是比较依据；用被改摘要登记回执得到
//     ErrConflict，用保存的服务摘要登记才成功。
//  2. 固定核对向量：用标识固定的合成包给出确定的规范 JSON 与确定摘要，
//     读者可照 README 用 sha256sum 手工复现；记录重排、同一时刻换时区
//     表示都不改变摘要。
//
// 本地核对只是接收方自己的检查：SubmitReceipt 只比较“提交摘要”与
// “交换保存的摘要”，摘要不符返回 ErrConflict，并不重新检查接收方手中
// 的临床内容。本地核对成功只说明手中内容与取包得到的服务摘要一致；
// 要完成这次交换，仍须按既有方式提交回执。
//
// 全程只使用合成患者资料，生效时间保留 9 位小数秒，正文含中文、换行、
// 引号、反斜杠、小于号与与号。运行：
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
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 固定的小数秒生效时刻（UTC），以及同一时刻的 +09:00 写法。
var (
	fixedEffectiveUTC  = time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC)
	fixedEffectiveZone = time.FixedZone("UTC+9", 9*60*60)
)

// 诊断与医嘱正文：均含中文、半角双引号、反斜杠、小于号、与号与换行。
const (
	fixedDiagContent  = "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历"
	fixedOrderContent = "医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注"
)

// 固定向量标识。
const (
	fixedPatientID   = "pat_digest_golden_01"
	fixedReceiverID  = "rcv_digest_golden"
	fixedEncounterID = "enc_digest_golden_01"
	fixedDiagRecID   = "rec_diag_golden"
	fixedDiagVerID   = "ver_diag_golden_2nd"
	fixedOrderRecID  = "rec_ord_golden"
	fixedOrderVerID  = "ver_ord_golden_7th"
)

// wantFixedCanonicalJSON 是固定向量参与摘要的精确规范字节（记录按
// record_id 排序后诊断在医嘱之前）。
const wantFixedCanonicalJSON = `{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}`

// 两个期望摘要均可用 sha256sum 对上面的规范字节独立复算（被改向量只
// 改动诊断正文最后一个汉字“历”→“厘”），不是抄自产品的计算结果。
const (
	wantFixedDigest         = "f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87"
	wantTamperedFixedDigest = "94a2ce6018d932de308ccabea9fd05aabe65087b034c5be87926c43e38b49a99"
)

var tamperedDiagContent = strings.Replace(fixedDiagContent, "历", "厘", 1)

func main() {
	runEndToEnd()
	runFixedVector()
}

// ---- 第一部分：真实调用链（包标识由存储随机生成，比较结论每次运行确定）----

func runEndToEnd() {
	dir, err := os.MkdirTemp("", "clinical-verify-digest-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	// 注入固定时钟，让两条记录在固定的小数秒时刻生效，便于对照时间字段；
	// 生产环境使用默认实时时钟，摘要规则完全相同。
	clock := fixedEffectiveUTC
	store, err := clinical.Open(dir, clinical.WithClock(func() time.Time { return clock }))
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor(fixedReceiverID)

	// 内部使用者准备一份“诊断 + 医嘱”的合成包（这部分只是包的来源，
	// 接收方核对时只认取包拿到的内容）。
	patient, err := store.RegisterPatient(doctor, "合成患者丙")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID,
		time.Date(2026, 3, 14, 6, 0, 0, 0, time.UTC))
	must("登记就诊", err)

	diagDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Diagnosis, fixedDiagContent)
	must("创建诊断草稿", err)
	if _, err := store.ActivateRecord(doctor, diagDraft.ID); err != nil {
		die("生效诊断: %v", err)
	}
	orderDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Order, fixedOrderContent)
	must("创建医嘱草稿", err)
	if _, err := store.ActivateRecord(doctor, orderDraft.ID); err != nil {
		die("生效医嘱: %v", err)
	}

	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis},
			{EncounterID: encounter.ID, Category: clinical.Order},
		},
		fixedEffectiveUTC.Add(-24*time.Hour), fixedEffectiveUTC.Add(24*time.Hour))
	must("建立授权", err)

	// 故意把医嘱排在诊断前面：摘要与入参/排列顺序无关。
	exchange, err := store.CreateExchange(doctor, patient.ID, receiver.ID,
		grant.ID, []clinical.ID{orderDraft.ID, diagDraft.ID}, "request-verify-digest")
	must("创建交换", err)

	// 接收方取包：一次拿到服务摘要与包内容。服务摘要是创建交换时算好、
	// 保存在交换上的值，取包只原样返回、不会重算。
	delivery, err := store.FetchPackage(receiver, exchange.ID)
	must("取包", err)
	serverDigest := delivery.Digest
	fmt.Println("==== 一、完整调用链 ====")
	fmt.Printf("取包成功：交换状态=%s，包内记录 %d 条\n", delivery.Status, len(delivery.Package.Records))
	puts("服务摘要已随取包取得并保存（接收方须原样保留，回执时回传）")

	// 情形一：正常输入——本地严格按内容重算，与服务摘要逐字相等。
	localDigest := digestOf(delivery.Package)
	fmt.Printf("正常内容：本地重算摘要与服务摘要一致=%v\n", localDigest == serverDigest)

	// 仅改变记录的排列顺序、或把同一时刻换一种时区表示，都不是内容改动。
	reordered := clinical.Package{
		PatientID:  delivery.Package.PatientID,
		ReceiverID: delivery.Package.ReceiverID,
		Records: []clinical.PackagedRecord{
			recordAsOf(delivery.Package.Records, clinical.Order, fixedEffectiveInPlus9()),
			recordAsOf(delivery.Package.Records, clinical.Diagnosis, fixedEffectiveInPlus9()),
		},
	}
	reorderedDigest := digestOf(reordered)
	fmt.Printf("记录重排且生效时间改写为 +09:00 同时刻表示后仍一致=%v\n", reorderedDigest == serverDigest)

	// 情形二：只改动收到的正文一个字（模拟留存/传输过程中的改动）。
	tampered := clonePackage(delivery.Package)
	diag := findRecord(tampered.Records, clinical.Diagnosis)
	diag.Content = strings.Replace(diag.Content, "历", "厘", 1)
	tamperedDigest := digestOf(tampered)
	fmt.Printf("正文改动一个字后：本地重算摘要与服务摘要一致=%v（应为 false）\n",
		tamperedDigest == serverDigest)

	// 核对与回执登记的关系：SubmitReceipt 只比对提交摘要与保存摘要，
	// 不重新检查接收方手中的临床内容——被改摘要被拒，交换状态与保存
	// 的服务摘要都不变。
	if _, err := store.SubmitReceipt(receiver, exchange.ID, tamperedDigest,
		clinical.ReceiptAccepted, ""); !errors.Is(err, clinical.ErrConflict) {
		die("被改摘要登记回执应返回 ErrConflict，实际得到 %v", err)
	}
	again, err := store.FetchPackage(receiver, exchange.ID)
	must("再次取包查看状态", err)
	fmt.Printf("用被改摘要登记回执：被拒绝（%v），交换状态仍为 %s，保存的服务摘要不变=%v\n",
		clinical.ErrConflict, again.Status, again.Digest == serverDigest)

	// 本地核对一致后，仍须按既有方式用保存的服务摘要完成回执。
	conf, err := store.SubmitReceipt(receiver, exchange.ID, serverDigest,
		clinical.ReceiptAccepted, "")
	must("用保存的服务摘要登记接受回执", err)
	fmt.Printf("用保存的服务摘要登记接受回执：成功，交换状态=%s，回执结果=%s\n", conf.Status, conf.Outcome)
}

func puts(msg string) { fmt.Println(msg) }

// ---- 第二部分：固定核对向量（标识与内容固定，摘要可手工复现）----

func runFixedVector() {
	fmt.Println("==== 二、固定核对向量（结果确定，可用 sha256sum 复现）====")

	// 接收方留存的一份合成交换包：刻意把医嘱排在前面，并把医嘱的生效
	// 时间写成 +09:00 时区的同一时刻——重算时仍应得到排序后、UTC 表示
	// 的同一份规范文档。
	pkg := clinical.Package{
		PatientID:  fixedPatientID,
		ReceiverID: fixedReceiverID,
		Records: []clinical.PackagedRecord{
			{
				EncounterID: fixedEncounterID,
				Category:    clinical.Order,
				RecordID:    fixedOrderRecID,
				VersionID:   fixedOrderVerID,
				Version:     7,
				EffectiveAt: time.Date(2026, 3, 14, 18, 7, 5, 123456789, fixedEffectiveZone),
				Content:     fixedOrderContent,
			},
			{
				EncounterID: fixedEncounterID,
				Category:    clinical.Diagnosis,
				RecordID:    fixedDiagRecID,
				VersionID:   fixedDiagVerID,
				Version:     2,
				EffectiveAt: fixedEffectiveUTC,
				Content:     fixedDiagContent,
			},
		},
	}

	digest, canonicalJSON, err := ComputePackageDigest(pkg)
	must("固定向量重算摘要", err)
	fmt.Printf("参与摘要的规范字节:\n%s\n", string(canonicalJSON))
	fmt.Printf("规范字节与固定向量逐字一致: %v\n", string(canonicalJSON) == wantFixedCanonicalJSON)
	fmt.Printf("期望摘要（sha256sum 独立复算）: %s\n", wantFixedDigest)
	fmt.Printf("本地根据内容算出的摘要:       %s\n", digest)
	fmt.Printf("核对是否一致: %v\n", digest == wantFixedDigest)

	// 只改正文一个字：摘要完全改变。
	tamperedPkg := clonePackage(pkg)
	tamperedPkg.Records[0].Content = tamperedDiagContent // clonePackage 已按标识排序，0 为诊断
	tamperedDigest, _, err := ComputePackageDigest(tamperedPkg)
	must("被改固定向量重算摘要", err)
	fmt.Printf("正文改动一个字后期望摘要: %s\n", wantTamperedFixedDigest)
	fmt.Printf("正文改动一个字后实算摘要: %s\n", tamperedDigest)
	fmt.Printf("被改摘要与原服务摘要一致: %v\n", tamperedDigest == wantFixedDigest)

	// 重排 + 换时区表示：摘要保持原值。
	shuffled := clinical.Package{
		PatientID:  pkg.PatientID,
		ReceiverID: pkg.ReceiverID,
		Records: []clinical.PackagedRecord{
			recordAsOf(pkg.Records, clinical.Order, fixedEffectiveInPlus9()),
			recordAsOf(pkg.Records, clinical.Diagnosis, fixedEffectiveInPlus9()),
		},
	}
	shuffledDigest, _, err := ComputePackageDigest(shuffled)
	must("重排/换时区固定向量重算摘要", err)
	fmt.Printf("重排并全部改用 +09:00 同时刻表示后摘要: %s（保持不变=%v）\n",
		shuffledDigest, shuffledDigest == wantFixedDigest)
}

// ComputePackageDigest 是接收方一侧的摘要核对函数：只根据取包得到的包
// 内容，按服务端创建交换时相同的规则重算摘要。它不访问存储、不检查
// 授权，是纯函数，可随时对留存的 PackageDelivery.Package 复算。
//
// 规则（详见 README“接收方核对包摘要”）：
//   - 输出一个 UTF-8、无多余空白、字段顺序固定的 JSON 文档；
//   - 顶层字段：patient_id(字符串)、receiver_id(字符串)、records(数组)；
//   - records 先按 record_id 的字符串升序排列；
//   - 每条记录字段顺序固定：encounter_id、category、record_id、
//     version_id 均为字符串，version 为 JSON 整数，effective_at 为
//     “先转 UTC 再按 RFC3339Nano 格式化”的字符串，content 为字符串；
//   - 字符串按 Go encoding/json 默认规则转义：双引号→\"、反斜杠→\\、
//     换行→\n 等控制字符转义，且默认 HTML 转义把 <、>、& 写成
//     \u003c、\u003e、\u0026；中文等非 ASCII 字符以 UTF-8 原样保留；
//   - 对上述字节序列求 SHA-256，编码为 64 位小写十六进制字符串。
func ComputePackageDigest(p clinical.Package) (digest string, canonical []byte, err error) {
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

	recs := make([]clinical.PackagedRecord, len(p.Records))
	copy(recs, p.Records)
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
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw, nil
}

func fixedEffectiveInPlus9() time.Time {
	return fixedEffectiveUTC.In(fixedEffectiveZone)
}

// digestOf 是 ComputePackageDigest 的便捷封装：本示例只需要摘要字符串，
// 规范字节仅在固定向量部分打印。参与序列化的都是字符串/整数等基础类型，
// 正常不应出错。
func digestOf(p clinical.Package) string {
	d, _, err := ComputePackageDigest(p)
	if err != nil {
		die("重算摘要失败: %v", err)
	}
	return d
}

func findRecord(recs []clinical.PackagedRecord, category string) *clinical.PackagedRecord {
	for i := range recs {
		if recs[i].Category == category {
			return &recs[i]
		}
	}
	return nil
}

// recordAsOf 取出指定类别的记录副本，并把生效时间改写为同一时刻的另一种
// 时区表示（用于演示时区表示无关）。
func recordAsOf(recs []clinical.PackagedRecord, category string, at time.Time) clinical.PackagedRecord {
	r := *findRecord(recs, category)
	r.EffectiveAt = at
	return r
}

func clonePackage(p clinical.Package) clinical.Package {
	q := clinical.Package{PatientID: p.PatientID, ReceiverID: p.ReceiverID}
	q.Records = append([]clinical.PackagedRecord(nil), p.Records...)
	sort.Slice(q.Records, func(i, j int) bool { return q.Records[i].RecordID < q.Records[j].RecordID })
	return q
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
